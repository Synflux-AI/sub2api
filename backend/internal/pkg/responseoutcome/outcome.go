// Package responseoutcome 从上游原始响应里提取"这次响应是怎么结束的"。
//
// 观测只读上游协议语义（Anthropic Messages / OpenAI Chat Completions /
// OpenAI Responses），必须在兼容转换之前喂入，避免转换层把 refusal /
// incomplete 映射成 stop 后丢失证据。观测结果只用于用量明细展示与筛选，
// 不参与计费、换号、SLA 或账号健康度判断。
package responseoutcome

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// 上游协议。
const (
	ProtocolAnthropicMessages     = "anthropic_messages"
	ProtocolOpenAIChatCompletions = "openai_chat_completions"
	ProtocolOpenAIResponses       = "openai_responses"
)

// 标准化结束原因，用于列表展示与筛选。原始值另存于 Outcome，不以此为唯一事实。
const (
	ReasonStop          = "stop"           // 正常结束：end_turn / stop_sequence / stop / completed
	ReasonToolUse       = "tool_use"       // 正常工具调用：tool_use / tool_calls / function_call
	ReasonPauseTurn     = "pause_turn"     // Anthropic 服务端工具循环暂停，需客户端续发
	ReasonTruncated     = "truncated"      // max_tokens / model_context_window_exceeded / length / max_output_tokens
	ReasonRefusal       = "refusal"        // 供应商明示的结构化拒答
	ReasonContentFilter = "content_filter" // 内容过滤
	ReasonIncomplete    = "incomplete"     // Responses incomplete 的其它原因（max_messages / steered / 未知）
	ReasonFailed        = "failed"         // Responses failed 或流内 error 事件
	ReasonCancelled     = "cancelled"      // Responses cancelled
	ReasonInterrupted   = "interrupted"    // 未观察到完整终态（断流、客户端取消等）
	ReasonEmpty         = "empty"          // 正常终态但没有任何可消费输出
	ReasonUnknown       = "unknown"        // 上游给出了未识别的原因值
)

// AbnormalReasons 是"异常结束"筛选集合。unknown 单独展示，不等同于故障；
// stop / tool_use / pause_turn 属于正常控制流。
var AbnormalReasons = []string{
	ReasonRefusal,
	ReasonContentFilter,
	ReasonTruncated,
	ReasonIncomplete,
	ReasonFailed,
	ReasonCancelled,
	ReasonInterrupted,
	ReasonEmpty,
}

// AllReasons 列出所有合法的标准化原因，供筛选参数校验。
var AllReasons = []string{
	ReasonStop,
	ReasonToolUse,
	ReasonPauseTurn,
	ReasonTruncated,
	ReasonRefusal,
	ReasonContentFilter,
	ReasonIncomplete,
	ReasonFailed,
	ReasonCancelled,
	ReasonInterrupted,
	ReasonEmpty,
	ReasonUnknown,
}

// IsValidReason 判断是否为合法的标准化原因。
func IsValidReason(reason string) bool {
	return slices.Contains(AllReasons, reason)
}

// FilterAbnormal 是筛选参数中代表"异常结束"集合的取值。
const FilterAbnormal = "abnormal"

// ParseFilter 解析逗号分隔的结束原因筛选参数，abnormal 展开为 AbnormalReasons
// （不含 unknown）。返回 error 表示存在非法取值；结果去重并保持出现顺序。
func ParseFilter(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(reason string) {
		if _, ok := seen[reason]; ok {
			return
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		switch {
		case part == "":
			continue
		case part == FilterAbnormal:
			for _, reason := range AbnormalReasons {
				add(reason)
			}
		case IsValidReason(part):
			add(part)
		default:
			return nil, fmt.Errorf("invalid finish_reason: %q", part)
		}
	}
	return out, nil
}

// rawValueMaxLen 限制原始原因值长度，防止异常上游把大字段塞进用量行。
const rawValueMaxLen = 64

// Outcome 是落库的结构化观测事实。所有字段都只包含协议枚举值或布尔标记，
// 不包含任何响应正文。
type Outcome struct {
	Protocol string `json:"protocol"`
	// Status 是 Responses 顶层 status（completed / incomplete / failed / cancelled）。
	Status string `json:"status,omitempty"`
	// StopReason 是请求级原始结束原因：Anthropic stop_reason 或 Chat 汇总选中的 finish_reason。
	StopReason string `json:"stop_reason,omitempty"`
	// ChoiceFinishReasons 仅在 Chat 有多个 choice 时记录，按 index 排列，未结束的 choice 为空串。
	ChoiceFinishReasons []string `json:"choice_finish_reasons,omitempty"`
	// IncompleteReason 是 Responses incomplete_details.reason。
	IncompleteReason string `json:"incomplete_reason,omitempty"`
	// StopDetailsType / StopDetailsCategory 来自 Anthropic refusal 的 stop_details。
	StopDetailsType     string `json:"stop_details_type,omitempty"`
	StopDetailsCategory string `json:"stop_details_category,omitempty"`
	// ErrorType 是流内 error 事件或 response.failed 中的错误类型/代码。
	ErrorType string `json:"error_type,omitempty"`
	// Refusal 表示观察到供应商明示的结构化拒答（stop_reason=refusal、
	// message.refusal / delta.refusal、Responses refusal 内容块或事件）。
	Refusal bool `json:"refusal,omitempty"`
	// ToolCall 表示观察到工具调用。
	ToolCall bool `json:"tool_call,omitempty"`
	// HasOutput 表示观察到可消费输出：非空文本、工具调用、拒答说明或图像/音频等非推理内容。
	HasOutput bool `json:"has_output"`
	// Terminal 表示观察到完整终态（Anthropic message_stop、Chat finish_reason、
	// Responses 终态事件或非流式完整响应体）。
	Terminal bool `json:"terminal"`
}

// FinishReason 按固定优先级把多项事实汇总为一个标准化原因：
// failed > cancelled > refusal > content_filter > interrupted > truncated >
// incomplete > pause_turn > tool_use > unknown > empty > stop。
// 拒答与截断同时出现时取拒答，截断事实仍保留在原始字段中。
func (o *Outcome) FinishReason() string {
	if o == nil {
		return ""
	}
	reasons := o.rawReasons()
	has := func(target string) bool { return slices.Contains(reasons, target) }
	switch {
	case has(ReasonFailed):
		return ReasonFailed
	case has(ReasonCancelled):
		return ReasonCancelled
	case o.Refusal || has(ReasonRefusal):
		return ReasonRefusal
	case has(ReasonContentFilter):
		return ReasonContentFilter
	case !o.Terminal:
		return ReasonInterrupted
	case has(ReasonTruncated):
		return ReasonTruncated
	case has(ReasonIncomplete):
		return ReasonIncomplete
	case has(ReasonPauseTurn):
		return ReasonPauseTurn
	case has(ReasonToolUse):
		return ReasonToolUse
	case has(ReasonUnknown):
		return ReasonUnknown
	case !o.HasOutput:
		return ReasonEmpty
	case has(ReasonStop):
		return ReasonStop
	default:
		return ReasonUnknown
	}
}

// rawReasons 把原始字段逐项映射为标准化原因（可能多个，例如多 choice）。
func (o *Outcome) rawReasons() []string {
	var out []string
	if o.ErrorType != "" {
		out = append(out, ReasonFailed)
	}
	switch o.Protocol {
	case ProtocolAnthropicMessages:
		if o.StopReason != "" {
			out = append(out, normalizeAnthropicStopReason(o.StopReason))
		}
	case ProtocolOpenAIChatCompletions:
		if len(o.ChoiceFinishReasons) > 0 {
			for _, r := range o.ChoiceFinishReasons {
				if r != "" {
					out = append(out, normalizeChatFinishReason(r))
				}
			}
		} else if o.StopReason != "" {
			out = append(out, normalizeChatFinishReason(o.StopReason))
		}
	case ProtocolOpenAIResponses:
		if o.Status != "" {
			out = append(out, normalizeResponsesStatus(o.Status, o.IncompleteReason, o.ToolCall))
		}
	}
	return out
}

func normalizeAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return ReasonStop
	case "tool_use":
		return ReasonToolUse
	case "pause_turn":
		return ReasonPauseTurn
	case "max_tokens", "model_context_window_exceeded":
		return ReasonTruncated
	case "refusal":
		return ReasonRefusal
	default:
		return ReasonUnknown
	}
}

func normalizeChatFinishReason(reason string) string {
	switch reason {
	case "stop":
		return ReasonStop
	case "length":
		return ReasonTruncated
	case "tool_calls", "function_call":
		return ReasonToolUse
	case "content_filter":
		return ReasonContentFilter
	default:
		return ReasonUnknown
	}
}

func normalizeResponsesStatus(status, incompleteReason string, toolCall bool) string {
	switch status {
	case "completed":
		if toolCall {
			return ReasonToolUse
		}
		return ReasonStop
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return ReasonTruncated
		case "content_filter":
			return ReasonContentFilter
		default:
			return ReasonIncomplete
		}
	case "failed":
		return ReasonFailed
	case "cancelled":
		return ReasonCancelled
	default:
		return ReasonUnknown
	}
}

// Observer 累积一次上游响应的观测事实。并发安全；nil Observer 的所有方法都是空操作，
// 因此调用点不需要判空。
type Observer struct {
	mu       sync.Mutex
	outcome  Outcome
	observed bool
	choices  map[int]string
	// pendingError 记录 Responses 流中尚未被终态事件确认的裸 error 事件。
	// 网关以之后到达的成功终态为准，因此裸 error 只有在没有任何终态时才落为 failed。
	pendingError string
}

// NewObserver 创建指定协议的观测器。
func NewObserver(protocol string) *Observer {
	return &Observer{outcome: Outcome{Protocol: protocol}}
}

// Snapshot 返回当前观测结果的副本；从未喂入任何上游数据时返回 nil，
// 使历史路径和未覆盖路径保持 NULL 而不是被默认成某个原因。
func (o *Observer) Snapshot() *Outcome {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.observed {
		return nil
	}
	out := o.outcome
	if !out.Terminal && o.pendingError != "" {
		out.ErrorType = o.pendingError
		out.Terminal = true
	}
	if len(o.choices) > 1 {
		maxIdx := 0
		for idx := range o.choices {
			if idx > maxIdx {
				maxIdx = idx
			}
		}
		out.ChoiceFinishReasons = make([]string, maxIdx+1)
		for idx, r := range o.choices {
			out.ChoiceFinishReasons[idx] = r
		}
	}
	return &out
}

// ObserveAnthropicPayload 自动区分非流式完整消息（type=message）与流式事件。
func (o *Observer) ObserveAnthropicPayload(payload []byte) {
	if o == nil || len(payload) == 0 {
		return
	}
	if gjson.GetBytes(payload, "type").String() == "message" {
		o.ObserveAnthropicMessage(payload)
		return
	}
	o.ObserveAnthropicEvent(payload)
}

// ObserveOpenAIPayload 自动区分 OpenAI 载荷形态：Responses 流式事件（带
// response.* / error 事件类型）、Responses 完整响应体、Chat Completions
// 流式 chunk 与完整响应体。eventType 为 SSE event 名，可为空。
func (o *Observer) ObserveOpenAIPayload(payload []byte, eventType string) {
	if o == nil || len(payload) == 0 {
		return
	}
	payloadType := gjson.GetBytes(payload, "type").String()
	if strings.HasPrefix(eventType, "response.") || strings.HasPrefix(payloadType, "response.") ||
		(payloadType == "error" && eventType != "") {
		o.ObserveResponsesEvent(payload)
		return
	}
	switch gjson.GetBytes(payload, "object").String() {
	case "chat.completion":
		o.ObserveChatCompletion(payload)
		return
	case "chat.completion.chunk":
		o.ObserveChatChunk(payload)
		return
	case "response":
		o.ObserveResponse(payload)
		return
	}
	if choices := gjson.GetBytes(payload, "choices"); choices.IsArray() {
		if gjson.GetBytes(payload, "choices.0.message").Exists() {
			o.ObserveChatCompletion(payload)
		} else {
			o.ObserveChatChunk(payload)
		}
		return
	}
	if gjson.GetBytes(payload, "status").Exists() && gjson.GetBytes(payload, "output").Exists() {
		o.ObserveResponse(payload)
	}
}

// ObserveAnthropicEvent 喂入一条 Anthropic Messages SSE 事件的 data JSON。
func (o *Observer) ObserveAnthropicEvent(data []byte) {
	if o == nil || len(data) == 0 {
		return
	}
	eventType := gjson.GetBytes(data, "type").String()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureProtocol(ProtocolAnthropicMessages)
	switch eventType {
	case "message_start":
		o.observed = true
		// message_start 中 stop_reason 恒为 null，不在此固化。
	case "content_block_start":
		o.observed = true
		o.observeAnthropicBlock(gjson.GetBytes(data, "content_block"))
	case "content_block_delta":
		o.observed = true
		delta := gjson.GetBytes(data, "delta")
		switch delta.Get("type").String() {
		case "text_delta":
			if delta.Get("text").String() != "" {
				o.outcome.HasOutput = true
			}
		case "input_json_delta":
			o.outcome.HasOutput = true
		}
	case "message_delta":
		o.observed = true
		o.setAnthropicStop(gjson.GetBytes(data, "delta"))
	case "message_stop":
		o.observed = true
		o.outcome.Terminal = true
	case "error":
		o.observed = true
		o.setErrorType(firstNonEmpty(
			gjson.GetBytes(data, "error.type").String(),
			"error",
		))
	}
}

// ObserveAnthropicMessage 喂入非流式 Anthropic Messages 完整响应体。
func (o *Observer) ObserveAnthropicMessage(body []byte) {
	if o == nil || len(body) == 0 {
		return
	}
	root := gjson.ParseBytes(body)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureProtocol(ProtocolAnthropicMessages)
	switch root.Get("type").String() {
	case "message":
	case "error":
		o.observed = true
		o.setErrorType(firstNonEmpty(root.Get("error.type").String(), "error"))
		return
	default:
		return
	}
	o.observed = true
	for _, block := range root.Get("content").Array() {
		o.observeAnthropicBlock(block)
	}
	o.setAnthropicStop(root)
	o.outcome.Terminal = true
}

func (o *Observer) observeAnthropicBlock(block gjson.Result) {
	switch block.Get("type").String() {
	case "", "thinking", "redacted_thinking":
		return
	case "text":
		// 流式 content_block_start 的 text 一般为空，正文由 text_delta 判断。
		if block.Get("text").String() != "" {
			o.outcome.HasOutput = true
		}
		return
	case "tool_use", "server_tool_use", "mcp_tool_use":
		o.outcome.ToolCall = true
	}
	o.outcome.HasOutput = true
}

func (o *Observer) setAnthropicStop(node gjson.Result) {
	reason := node.Get("stop_reason").String()
	if reason == "" || o.outcome.StopReason != "" {
		return
	}
	o.outcome.StopReason = truncateRaw(reason)
	if reason == "refusal" {
		o.outcome.Refusal = true
	}
	if details := node.Get("stop_details"); details.IsObject() {
		o.outcome.StopDetailsType = truncateRaw(details.Get("type").String())
		o.outcome.StopDetailsCategory = truncateRaw(details.Get("category").String())
	}
}

// ObserveChatChunk 喂入一条 OpenAI Chat Completions 流式 chunk 的 data JSON。
// "[DONE]" 哨兵不需要喂入；终态以 finish_reason 为准。
func (o *Observer) ObserveChatChunk(data []byte) {
	if o == nil || len(data) == 0 {
		return
	}
	root := gjson.ParseBytes(data)
	if !root.IsObject() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureProtocol(ProtocolOpenAIChatCompletions)
	if errNode := root.Get("error"); errNode.Exists() && errNode.Type != gjson.Null {
		o.observed = true
		o.setErrorType(firstNonEmpty(errNode.Get("type").String(), errNode.Get("code").String(), "error"))
		return
	}
	o.observed = true
	// 最终 usage chunk 的 choices 为空数组，不会覆盖已采集的结束原因。
	for _, choice := range root.Get("choices").Array() {
		o.observeChatMessage(choice.Get("delta"))
		o.setChatFinish(int(choice.Get("index").Int()), choice.Get("finish_reason").String())
	}
}

// ObserveChatCompletion 喂入非流式 OpenAI Chat Completions 完整响应体。
func (o *Observer) ObserveChatCompletion(body []byte) {
	if o == nil || len(body) == 0 {
		return
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureProtocol(ProtocolOpenAIChatCompletions)
	if errNode := root.Get("error"); errNode.Exists() && errNode.Type != gjson.Null {
		o.observed = true
		o.setErrorType(firstNonEmpty(errNode.Get("type").String(), errNode.Get("code").String(), "error"))
		return
	}
	choices := root.Get("choices").Array()
	if len(choices) == 0 {
		return
	}
	o.observed = true
	for _, choice := range choices {
		o.observeChatMessage(choice.Get("message"))
		o.setChatFinish(int(choice.Get("index").Int()), choice.Get("finish_reason").String())
	}
}

func (o *Observer) observeChatMessage(msg gjson.Result) {
	if !msg.Exists() {
		return
	}
	if msg.Get("refusal").String() != "" {
		o.outcome.Refusal = true
		o.outcome.HasOutput = true
	}
	if content := msg.Get("content"); content.Exists() {
		switch {
		case content.Type == gjson.String && content.String() != "":
			o.outcome.HasOutput = true
		case content.IsArray() && len(content.Array()) > 0:
			o.outcome.HasOutput = true
		}
	}
	if tc := msg.Get("tool_calls"); tc.IsArray() && len(tc.Array()) > 0 {
		o.outcome.ToolCall = true
		o.outcome.HasOutput = true
	}
	if fc := msg.Get("function_call"); fc.IsObject() {
		o.outcome.ToolCall = true
		o.outcome.HasOutput = true
	}
	if audio := msg.Get("audio"); audio.IsObject() {
		o.outcome.HasOutput = true
	}
	if images := msg.Get("images"); images.IsArray() && len(images.Array()) > 0 {
		o.outcome.HasOutput = true
	}
}

func (o *Observer) setChatFinish(index int, reason string) {
	if reason == "" {
		return
	}
	if o.choices == nil {
		o.choices = make(map[int]string, 1)
	}
	if _, seen := o.choices[index]; seen {
		return
	}
	o.choices[index] = truncateRaw(reason)
	o.outcome.Terminal = true
	// 请求级 StopReason 取汇总优先级最高的 choice 原因。
	best := ""
	bestRank := -1
	for _, r := range o.choices {
		if rank := chatReasonRank(r); rank > bestRank {
			best, bestRank = r, rank
		}
	}
	o.outcome.StopReason = best
}

func chatReasonRank(reason string) int {
	switch normalizeChatFinishReason(reason) {
	case ReasonContentFilter:
		return 5
	case ReasonTruncated:
		return 4
	case ReasonToolUse:
		return 3
	case ReasonUnknown:
		return 2
	default:
		return 1
	}
}

// ObserveResponsesEvent 喂入一条 OpenAI Responses 流式事件（SSE data 或 WS 帧）的 JSON。
func (o *Observer) ObserveResponsesEvent(data []byte) {
	if o == nil || len(data) == 0 {
		return
	}
	eventType := gjson.GetBytes(data, "type").String()
	if eventType == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureProtocol(ProtocolOpenAIResponses)
	switch eventType {
	case "response.created", "response.in_progress", "response.queued":
		o.observed = true
	case "response.output_text.delta":
		o.observed = true
		if gjson.GetBytes(data, "delta").String() != "" {
			o.outcome.HasOutput = true
		}
	case "response.refusal.delta", "response.refusal.done":
		o.observed = true
		o.outcome.Refusal = true
		o.outcome.HasOutput = true
	case "response.output_item.added", "response.output_item.done":
		o.observed = true
		o.observeResponsesItem(gjson.GetBytes(data, "item"))
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled":
		o.observed = true
		o.observeResponseObject(gjson.GetBytes(data, "response"))
	case "error":
		o.observed = true
		if !o.outcome.Terminal && o.pendingError == "" {
			o.pendingError = truncateRaw(firstNonEmpty(
				gjson.GetBytes(data, "error.type").String(),
				gjson.GetBytes(data, "error.code").String(),
				gjson.GetBytes(data, "code").String(),
				"error",
			))
		}
	}
}

// ObserveResponse 喂入非流式 OpenAI Responses 完整响应体。
func (o *Observer) ObserveResponse(body []byte) {
	if o == nil || len(body) == 0 {
		return
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || !root.Get("status").Exists() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureProtocol(ProtocolOpenAIResponses)
	o.observed = true
	o.observeResponseObject(root)
}

func (o *Observer) observeResponseObject(resp gjson.Result) {
	if !resp.Exists() {
		return
	}
	for _, item := range resp.Get("output").Array() {
		o.observeResponsesItem(item)
	}
	// 重复终态只认第一个。
	if o.outcome.Terminal {
		return
	}
	status := resp.Get("status").String()
	if status == "" || status == "in_progress" || status == "queued" {
		return
	}
	o.outcome.Status = truncateRaw(status)
	o.outcome.IncompleteReason = truncateRaw(resp.Get("incomplete_details.reason").String())
	if status == "failed" {
		o.setErrorType(firstNonEmpty(resp.Get("error.code").String(), resp.Get("error.type").String(), o.pendingError, "failed"))
	}
	o.pendingError = ""
	o.outcome.Terminal = true
}

func (o *Observer) observeResponsesItem(item gjson.Result) {
	switch item.Get("type").String() {
	case "", "reasoning", "compaction":
		return
	case "message":
		for _, part := range item.Get("content").Array() {
			switch part.Get("type").String() {
			case "refusal":
				o.outcome.Refusal = true
				o.outcome.HasOutput = true
			case "output_text":
				if part.Get("text").String() != "" {
					o.outcome.HasOutput = true
				}
			}
		}
		return
	case "function_call", "custom_tool_call", "local_shell_call", "shell_call", "apply_patch_call", "computer_call", "mcp_call":
		o.outcome.ToolCall = true
	}
	o.outcome.HasOutput = true
}

func (o *Observer) ensureProtocol(protocol string) {
	if o.outcome.Protocol == "" {
		o.outcome.Protocol = protocol
	}
}

func (o *Observer) setErrorType(errType string) {
	if o.outcome.ErrorType == "" {
		o.outcome.ErrorType = truncateRaw(errType)
	}
}

func truncateRaw(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > rawValueMaxLen {
		return value[:rawValueMaxLen]
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
