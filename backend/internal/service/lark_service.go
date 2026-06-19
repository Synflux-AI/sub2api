package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	larkWebhookBase   = "https://open.feishu.cn/open-apis/bot/v2/hook/"
	larkAPIBase       = "https://open.feishu.cn/open-apis"
	larkTokenURL      = "/auth/v3/tenant_access_token/internal"
	larkSendMsgURL    = "/im/v1/messages"
	larkHTTPTimeout   = 10 * time.Second
	larkTokenCacheTTL = 110 * time.Minute // token TTL is 2h, refresh 10min early
)

// LarkMsgType represents supported Lark message types.
type LarkMsgType string

const (
	LarkMsgTypeText LarkMsgType = "text"
	LarkMsgTypePost LarkMsgType = "post"        // rich text / 富文本
	LarkMsgTypeCard LarkMsgType = "interactive" // card / 卡片
)

// LarkService sends notifications to Lark (Feishu) via webhook or app API.
type LarkService struct {
	httpClient *http.Client

	mu             sync.Mutex
	cachedToken    string
	tokenExpiresAt time.Time
}

func NewLarkService() *LarkService {
	return &LarkService{
		httpClient: &http.Client{Timeout: larkHTTPTimeout},
	}
}

// PaymentOrderNotifyInfo holds the data needed to build a payment order notification card.
type PaymentOrderNotifyInfo struct {
	OrderID     int64
	UserEmail   string
	UserName    string
	OrderType   string // "balance" or "subscription"
	Amount      float64
	PayAmount   float64
	PaymentType string
	CompletedAt time.Time
}

// ========== Public send methods ==========

// SendPaymentOrderCard sends a payment order completion notification as an interactive card.
func (s *LarkService) SendPaymentOrderCard(ctx context.Context, cfg *OpsLarkNotificationConfig, order *PaymentOrderNotifyInfo) error {
	if s == nil || cfg == nil || !cfg.Enabled || order == nil {
		return nil
	}
	card := buildPaymentOrderCard(order)
	return s.sendCard(ctx, cfg, card)
}

// SendAlertCard sends an ops alert as a Lark interactive card.
func (s *LarkService) SendAlertCard(ctx context.Context, cfg *OpsLarkNotificationConfig, rule *OpsAlertRule, event *OpsAlertEvent) error {
	if s == nil || cfg == nil || !cfg.Enabled {
		return nil
	}
	card := buildAlertCard(rule, event)
	return s.sendCard(ctx, cfg, card)
}

// SendAccountAnomalyCard sends an account anomaly notification as an interactive card.
func (s *LarkService) SendAccountAnomalyCard(ctx context.Context, cfg *OpsLarkNotificationConfig, accountName, platform, status, reason string) error {
	if s == nil || cfg == nil || !cfg.Enabled {
		return nil
	}
	card := buildAccountAnomalyCard(accountName, platform, status, reason)
	return s.sendCard(ctx, cfg, card)
}

// SendTestMessage sends a plain text test message.
func (s *LarkService) SendTestMessage(ctx context.Context, cfg *OpsLarkNotificationConfig, text string) error {
	if s == nil || cfg == nil {
		return errors.New("lark service or config not initialized")
	}
	if strings.TrimSpace(text) == "" {
		text = "This is a test message from sub2api."
	}
	payload := map[string]any{
		"msg_type": string(LarkMsgTypeText),
		"content":  map[string]string{"text": text},
	}
	return s.dispatch(ctx, cfg, payload)
}

// ========== Internal helpers ==========

func (s *LarkService) sendCard(ctx context.Context, cfg *OpsLarkNotificationConfig, card map[string]any) error {
	payload := map[string]any{
		"msg_type": string(LarkMsgTypeCard),
		"card":     card,
	}
	return s.dispatch(ctx, cfg, payload)
}

// dispatch routes to webhook or app-based sending.
func (s *LarkService) dispatch(ctx context.Context, cfg *OpsLarkNotificationConfig, payload map[string]any) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "app":
		return s.sendViaApp(ctx, cfg, payload)
	default: // "webhook" or empty
		return s.sendViaWebhook(ctx, cfg, payload)
	}
}

// sendViaWebhook posts to a custom bot webhook URL.
func (s *LarkService) sendViaWebhook(ctx context.Context, cfg *OpsLarkNotificationConfig, payload map[string]any) error {
	webhookURL := strings.TrimSpace(cfg.WebhookURL)
	if webhookURL == "" {
		return errors.New("lark webhook URL is not configured")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("lark: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("lark: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lark: send webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("lark: webhook returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Check Lark API response code.
	var larkResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if jerr := json.Unmarshal(respBody, &larkResp); jerr == nil && larkResp.Code != 0 {
		return fmt.Errorf("lark: webhook error code=%d msg=%s", larkResp.Code, larkResp.Msg)
	}
	return nil
}

// sendViaApp sends a message to a Lark chat via App API (tenant access token).
func (s *LarkService) sendViaApp(ctx context.Context, cfg *OpsLarkNotificationConfig, payload map[string]any) error {
	if strings.TrimSpace(cfg.AppID) == "" || strings.TrimSpace(cfg.AppSecret) == "" {
		return errors.New("lark app ID and secret are required for app mode")
	}
	if strings.TrimSpace(cfg.ReceiveID) == "" {
		return errors.New("lark receive_id (chat_id or open_id) is required for app mode")
	}

	token, err := s.getTenantAccessToken(ctx, cfg.AppID, cfg.AppSecret)
	if err != nil {
		return fmt.Errorf("lark: get token: %w", err)
	}

	receiveIDType := strings.TrimSpace(cfg.ReceiveIDType)
	if receiveIDType == "" {
		receiveIDType = "chat_id"
	}

	msgType := LarkMsgTypeText
	var contentStr string

	if mt, ok := payload["msg_type"].(string); ok {
		msgType = LarkMsgType(mt)
	}

	switch msgType {
	case LarkMsgTypeCard:
		if card, ok := payload["card"]; ok {
			b, _ := json.Marshal(card)
			contentStr = string(b)
		}
	case LarkMsgTypePost:
		if content, ok := payload["content"]; ok {
			b, _ := json.Marshal(content)
			contentStr = string(b)
		}
	default:
		if content, ok := payload["content"]; ok {
			b, _ := json.Marshal(content)
			contentStr = string(b)
		}
	}

	apiPayload := map[string]any{
		"receive_id": strings.TrimSpace(cfg.ReceiveID),
		"msg_type":   string(msgType),
		"content":    contentStr,
	}

	body, err := json.Marshal(apiPayload)
	if err != nil {
		return fmt.Errorf("lark: marshal api payload: %w", err)
	}

	url := larkAPIBase + larkSendMsgURL + "?receive_id_type=" + receiveIDType
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("lark: create api request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lark: send app message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("lark: app API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var larkResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if jerr := json.Unmarshal(respBody, &larkResp); jerr == nil && larkResp.Code != 0 {
		return fmt.Errorf("lark: app API error code=%d msg=%s", larkResp.Code, larkResp.Msg)
	}
	return nil
}

// getTenantAccessToken fetches (and caches) a tenant access token.
func (s *LarkService) getTenantAccessToken(ctx context.Context, appID, appSecret string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cachedToken != "" && time.Now().Before(s.tokenExpiresAt) {
		return s.cachedToken, nil
	}

	payload := map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, larkAPIBase+larkTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var tokenResp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return "", fmt.Errorf("lark: parse token response: %w", err)
	}
	if tokenResp.Code != 0 {
		return "", fmt.Errorf("lark: get token error code=%d msg=%s", tokenResp.Code, tokenResp.Msg)
	}

	s.cachedToken = tokenResp.TenantAccessToken
	ttl := time.Duration(tokenResp.Expire) * time.Second
	if ttl <= 0 {
		ttl = larkTokenCacheTTL
	} else if ttl > larkTokenCacheTTL {
		ttl = larkTokenCacheTTL
	}
	s.tokenExpiresAt = time.Now().Add(ttl)
	return s.cachedToken, nil
}

// ========== Card builders ==========

func buildAlertCard(rule *OpsAlertRule, event *OpsAlertEvent) map[string]any {
	severity := "-"
	ruleName := "-"
	if rule != nil {
		severity = strings.TrimSpace(rule.Severity)
		ruleName = strings.TrimSpace(rule.Name)
	}

	var elements []any
	if event != nil && event.Breakdown != nil {
		// 错误率类告警:渲染业务上下文卡片(指标色块 + 平台 + Top 用户/错误/上游 + 样例)。
		elements = buildAlertRichElements(rule, event)
	} else {
		// 其它指标(CPU/账号数等)或无明细:渲染基础卡片。
		elements = buildAlertSimpleElements(rule, event)
	}

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true, "enable_forward": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": fmt.Sprintf("%s %s", alertSeverityEmoji(severity), ruleName)},
			"subtitle": map[string]any{"tag": "plain_text", "content": fmt.Sprintf("%s · sub2api 实时告警", severity)},
			"template": alertHeaderColor(severity),
		},
		"elements": elements,
	}
}

// buildAlertSimpleElements 渲染基础告警卡片(无业务明细时的回退样式)。
func buildAlertSimpleElements(rule *OpsAlertRule, event *OpsAlertEvent) []any {
	severity := "-"
	description := "-"
	metricValue := "-"
	firedAt := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	if rule != nil {
		severity = strings.TrimSpace(rule.Severity)
	}
	if event != nil {
		description = strings.TrimSpace(event.Description)
		firedAt = event.FiredAt.UTC().Format("2006-01-02 15:04:05 UTC")
		if event.MetricValue != nil {
			metricValue = fmt.Sprintf("%.4f", *event.MetricValue)
		}
	}
	return []any{
		map[string]any{
			"tag": "div",
			"fields": []any{
				map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Severity**\n%s", severity)}},
				map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Metric Value**\n%s", metricValue)}},
			},
		},
		map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Description**\n%s", description)}},
		map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Fired At**\n%s", firedAt)}},
		map[string]any{"tag": "hr"},
		map[string]any{"tag": "note", "elements": []any{map[string]any{"tag": "plain_text", "content": "sub2api ops alert"}}},
	}
}

// buildAlertRichElements 渲染含业务上下文的错误率类告警卡片。
func buildAlertRichElements(rule *OpsAlertRule, event *OpsAlertEvent) []any {
	bd := event.Breakdown
	win := bd.WindowMinutes
	if win <= 0 {
		win = 1
	}
	scope := alertScopeLabel(event.Dimensions)

	// 指标色块:错误数(下方灰字=请求总数+窗口) | 指标值(下方灰字=阈值+范围)
	errSub := fmt.Sprintf("共 %d 请求 · 近 %d 分钟", bd.WindowRequests, win)
	metricLabel := alertMetricLabel(rule)
	metricVal := "-"
	if event.MetricValue != nil {
		metricVal = fmt.Sprintf("%.1f%%", *event.MetricValue)
	}
	metricSub := scope
	if rule != nil {
		metricSub = fmt.Sprintf("阈值 %s %.0f%% · %s", strings.TrimSpace(rule.Operator), rule.Threshold, scope)
	}
	statTiles := map[string]any{
		"tag":                "column_set",
		"flex_mode":          "stretch",
		"background_style":   "grey",
		"horizontal_spacing": "default",
		"columns": []any{
			alertStatColumn("错误数", fmt.Sprintf("<font color='red'>**%d**</font>", bd.TotalErrors), errSub),
			alertStatColumn(metricLabel, fmt.Sprintf("<font color='red'>**%s**</font>", metricVal), metricSub),
		},
	}

	elements := []any{statTiles}

	// 平台分布
	if len(bd.Platforms) > 0 {
		parts := make([]string, 0, len(bd.Platforms))
		for i, p := range bd.Platforms {
			parts = append(parts, fmt.Sprintf("<font color='%s'>%s %d</font>", alertCountColor(i), alertPlatformDisplay(p.Platform), p.Count))
		}
		elements = append(elements, map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": "🌐 **平台分布**　" + strings.Join(parts, "　·　")},
		})
	}

	// 一句话业务洞察:4xx/5xx 归因
	if insight := alertInsightLine(bd); insight != "" {
		elements = append(elements, map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": insight},
		})
	}

	elements = append(elements, map[string]any{"tag": "hr"})

	// 👤 触发用户 TOP(含各自错误构成)
	if len(bd.TopUsers) > 0 {
		var b strings.Builder
		b.WriteString("**👤 触发用户 TOP**（含各自错误构成）")
		for i, u := range bd.TopUsers {
			comp := ""
			if len(u.Errors) > 0 {
				cs := make([]string, 0, len(u.Errors))
				for _, e := range u.Errors {
					cs = append(cs, fmt.Sprintf("%s ×%d", alertErrorShort(e), e.Count))
				}
				comp = fmt.Sprintf("　<font color='grey'>%s</font>", strings.Join(cs, " / "))
			}
			fmt.Fprintf(&b, "\n• %s — <font color='%s'>**%d**</font>%s", alertUserLabel(u), alertCountColor(i), u.Count, comp)
		}
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": b.String()}})
	}

	// 🧩 错误类型 TOP
	if len(bd.TopErrorTypes) > 0 {
		var b strings.Builder
		b.WriteString("**🧩 错误类型 TOP**")
		for i, e := range bd.TopErrorTypes {
			fmt.Fprintf(&b, "\n• %s — <font color='%s'>**%d**</font>", alertErrorTypeLabel(e), alertCountColor(i), e.Count)
		}
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": b.String()}})
	}

	// 🛰️ 上游渠道 TOP(平台 · 渠道名 · 模型)
	if len(bd.TopUpstreams) > 0 {
		var b strings.Builder
		b.WriteString("**🛰️ 上游渠道 TOP**（平台 · 渠道 · 模型）")
		for i, up := range bd.TopUpstreams {
			if up.AccountID <= 0 {
				fmt.Fprintf(&b, "\n• <font color='grey'>无上游（客户端错误，未到选号）— %d</font>", up.Count)
				continue
			}
			fmt.Fprintf(&b, "\n• %s — <font color='%s'>**%d**</font>", alertUpstreamLabel(up), alertCountColor(i), up.Count)
		}
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": b.String()}})
	}

	// 📋 样例报错
	if len(bd.Samples) > 0 {
		var b strings.Builder
		b.WriteString("**📋 样例报错**")
		for _, s := range bd.Samples {
			fmt.Fprintf(&b, "\n`%d` %s", s.StatusCode, truncateAlertSample(s.Message, 160))
		}
		elements = append(elements,
			map[string]any{"tag": "hr"},
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": b.String()}},
		)
	}

	firedAt := event.FiredAt.UTC().Format("2006-01-02 15:04:05 UTC")
	elements = append(elements, map[string]any{
		"tag":      "note",
		"elements": []any{map[string]any{"tag": "plain_text", "content": "🤖 sub2api ops alert · Fired at " + firedAt}},
	})
	return elements
}

// alertStatColumn 构造一个指标色块列(标题 / 大号值 / 灰色副说明,居中)。
func alertStatColumn(title, value, sub string) map[string]any {
	content := fmt.Sprintf("**%s**\n%s", title, value)
	if sub != "" {
		content += fmt.Sprintf("\n<font color='grey'>%s</font>", sub)
	}
	return map[string]any{
		"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "center",
		"elements": []any{map[string]any{"tag": "markdown", "text_align": "center", "content": content}},
	}
}

// alertInsightLine 根据 4xx/5xx 拆分给出归因提示。
func alertInsightLine(bd *OpsAlertBreakdown) string {
	if bd == nil || (bd.Client4xx == 0 && bd.Server5xx == 0) {
		return ""
	}
	hint := ""
	switch {
	case bd.Client4xx > bd.Server5xx*2:
		hint = "大概率是个别用户请求异常，而非上游故障"
	case bd.Server5xx > bd.Client4xx:
		hint = "以上游/网关 5xx 为主，关注上游可用性"
	default:
		hint = "客户端与上游错误并存，需分别排查"
	}
	return fmt.Sprintf("⚠️ 客户端 `4xx` <font color='orange'>**%d**</font> 条 · 上游 `5xx` <font color='red'>**%d**</font> 条 —— %s。", bd.Client4xx, bd.Server5xx, hint)
}

// alertUserLabel 渲染用户标识:有备注则「备注 · 邮箱」,否则邮箱,再否则 user#id。
func alertUserLabel(u OpsAlertUserStat) string {
	email := strings.TrimSpace(u.Email)
	notes := strings.TrimSpace(u.Notes)
	switch {
	case notes != "" && email != "":
		return fmt.Sprintf("%s · %s", notes, email)
	case email != "":
		return email
	case notes != "":
		return notes
	default:
		return fmt.Sprintf("user#%d", u.UserID)
	}
}

// alertErrorShort 错误构成的简短标签(优先 error_type,否则状态码)。
func alertErrorShort(e OpsAlertErrorTypeStat) string {
	if strings.TrimSpace(e.ErrorType) != "" {
		return strings.TrimSpace(e.ErrorType)
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return "unknown"
}

// alertErrorTypeLabel 错误类型榜的标签:error_type + 状态码,上游码不同则补充。
func alertErrorTypeLabel(e OpsAlertErrorTypeStat) string {
	et := strings.TrimSpace(e.ErrorType)
	if et == "" {
		et = "unknown"
	}
	label := et
	if e.StatusCode > 0 {
		label = fmt.Sprintf("%s %d", et, e.StatusCode)
	}
	if e.UpstreamStatusCode > 0 && e.UpstreamStatusCode != e.StatusCode {
		label = fmt.Sprintf("%s ← 上游 %d", label, e.UpstreamStatusCode)
	}
	return "`" + label + "`"
}

// alertUpstreamLabel 上游榜标签:平台 · 渠道名 · 模型。
func alertUpstreamLabel(up OpsAlertUpstreamStat) string {
	segs := make([]string, 0, 3)
	if p := alertPlatformDisplay(up.Platform); p != "" {
		segs = append(segs, p)
	}
	if name := strings.TrimSpace(up.AccountName); name != "" {
		segs = append(segs, name)
	} else if up.AccountID > 0 {
		segs = append(segs, fmt.Sprintf("acct#%d", up.AccountID))
	}
	if m := strings.TrimSpace(up.Model); m != "" {
		segs = append(segs, m)
	}
	if len(segs) == 0 {
		return "unknown"
	}
	return strings.Join(segs, " · ")
}

// alertPlatformDisplay 平台名展示:已知平台规范化大小写。
func alertPlatformDisplay(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "":
		return ""
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "gemini", "google":
		return "Gemini"
	default:
		return strings.TrimSpace(p)
	}
}

// alertScopeLabel 从事件维度还原范围说明(platform/group),无则 overall。
func alertScopeLabel(dims map[string]any) string {
	if len(dims) == 0 {
		return "overall"
	}
	parts := make([]string, 0, 2)
	if p, ok := dims["platform"]; ok {
		if s := strings.TrimSpace(fmt.Sprintf("%v", p)); s != "" {
			parts = append(parts, "platform="+s)
		}
	}
	if g, ok := dims["group_id"]; ok {
		parts = append(parts, fmt.Sprintf("group=%v", g))
	}
	if len(parts) == 0 {
		return "overall"
	}
	return strings.Join(parts, " ")
}

// alertMetricLabel 指标类型的中文展示名。
func alertMetricLabel(rule *OpsAlertRule) string {
	if rule == nil {
		return "指标"
	}
	switch strings.ToLower(strings.TrimSpace(rule.MetricType)) {
	case "error_rate":
		return "错误率"
	case "success_rate":
		return "成功率"
	case "upstream_error_rate":
		return "上游错误率"
	default:
		return strings.TrimSpace(rule.MetricType)
	}
}

// alertCountColor 按排名给计数着色:第 1 红、第 2 橙、其余默认。
func alertCountColor(rank int) string {
	switch rank {
	case 0:
		return "red"
	case 1:
		return "orange"
	default:
		return "default"
	}
}

// alertHeaderColor 卡片头部颜色:P0/P1 红、P2 橙、其余蓝。
func alertHeaderColor(severity string) string {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "P0", "P1", "CRITICAL":
		return "red"
	case "P2", "WARNING":
		return "orange"
	case "P3", "INFO":
		return "blue"
	default:
		return "red"
	}
}

// alertSeverityEmoji 头部标题前缀 emoji。
func alertSeverityEmoji(severity string) string {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "P0", "P1", "CRITICAL":
		return "🚨"
	case "P2", "WARNING":
		return "⚠️"
	default:
		return "🔔"
	}
}

func truncateAlertSample(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if max > 0 && len([]rune(s)) > max {
		return string([]rune(s)[:max]) + "…"
	}
	return s
}

func buildAccountAnomalyCard(accountName, platform, status, reason string) map[string]any {
	if accountName == "" {
		accountName = "-"
	}
	if platform == "" {
		platform = "-"
	}
	if status == "" {
		status = "-"
	}
	if reason == "" {
		reason = "-"
	}

	headerColor := "yellow"
	if strings.ToLower(status) == "error" {
		headerColor = "red"
	}

	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": fmt.Sprintf("[Account Anomaly] %s", accountName)},
			"template": headerColor,
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"fields": []any{
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Account**\n%s", accountName)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Platform**\n%s", platform)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Status**\n%s", status)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Time**\n%s", now)},
					},
				},
			},
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": fmt.Sprintf("**Reason**\n%s", reason),
				},
			},
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag": "note",
				"elements": []any{
					map[string]any{"tag": "plain_text", "content": "sub2api account monitor"},
				},
			},
		},
	}
}

func buildPaymentOrderCard(o *PaymentOrderNotifyInfo) map[string]any {
	orderTypeLabel := "Balance Recharge"
	if o.OrderType == "subscription" {
		orderTypeLabel = "Subscription Purchase"
	}
	displayName := o.UserName
	if displayName == "" {
		displayName = o.UserEmail
	}
	completedAt := o.CompletedAt.UTC().Format("2006-01-02 15:04:05 UTC")

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": fmt.Sprintf("[Payment] %s", orderTypeLabel)},
			"template": "green",
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"fields": []any{
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Order ID**\n%d", o.OrderID)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Type**\n%s", orderTypeLabel)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**User**\n%s", displayName)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Payment**\n%s", o.PaymentType)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Amount**\n%.2f", o.Amount)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Paid**\n%.2f", o.PayAmount)},
					},
				},
			},
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": fmt.Sprintf("**Completed At**\n%s", completedAt),
				},
			},
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag": "note",
				"elements": []any{
					map[string]any{"tag": "plain_text", "content": "sub2api payment notify"},
				},
			},
		},
	}
}
