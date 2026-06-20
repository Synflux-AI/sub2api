package service

import (
	"strings"
	"testing"
)

func sampleBreakdown() *OpsAlertBreakdown {
	return &OpsAlertBreakdown{
		WindowMinutes:  5,
		WindowRequests: 81,
		TotalErrors:    18,
		Client4xx:      17,
		Server5xx:      1,
		Platforms: []OpsAlertPlatformStat{
			{Platform: "anthropic", Count: 10},
			{Platform: "openai", Count: 8},
		},
		TopUsers: []OpsAlertUserStat{
			{UserID: 64, Email: "e77938803@gmail.com", Notes: "田良智@云联精灵", Count: 5,
				Errors: []OpsAlertErrorTypeStat{{ErrorType: "invalid_request_error", StatusCode: 400, Count: 4}, {ErrorType: "api_error", StatusCode: 400, Count: 1}}},
			{UserID: 99, Count: 4}, // 无 email/notes,回退 user#id
		},
		TopErrorTypes: []OpsAlertErrorTypeStat{
			{ErrorType: "invalid_request_error", StatusCode: 400, Count: 12},
			{ErrorType: "upstream_error", StatusCode: 502, UpstreamStatusCode: 503, Count: 1},
		},
		TopUpstreams: []OpsAlertUpstreamStat{
			{AccountID: 3, AccountName: "crs15-max", Platform: "anthropic", Model: "claude-haiku-4-5", Count: 4},
			{AccountID: 0, Count: 12}, // 无上游
		},
		Samples: []OpsAlertSampleStat{
			{StatusCode: 400, Message: "Failed to read request body"},
			{StatusCode: 502, Message: "context canceled\nupstream"},
		},
	}
}

func TestBuildAlertRichElements(t *testing.T) {
	rule := &OpsAlertRule{Name: "错误率极高", Severity: "P0", MetricType: "error_rate", Operator: ">", Threshold: 20}
	mv := 22.2
	event := &OpsAlertEvent{Severity: "P0", MetricValue: &mv, Breakdown: sampleBreakdown()}
	card := buildAlertCard(rule, event)

	// 把整张卡片拍平成字符串做包含断言
	flat := flattenAny(card)
	for _, want := range []string{
		"错误率极高",
		"共 81 请求 · 近 5 分钟",
		"Anthropic 10",
		"OpenAI 8",
		"田良智@云联精灵 · e77938803@gmail.com",
		"invalid_request_error ×4 / api_error ×1",
		"user#99",
		"`invalid_request_error 400`",
		"crs15-max",
		"无上游（客户端错误，未到选号）— 12",
		"Failed to read request body",
		"客户端 `4xx`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("rich card missing %q", want)
		}
	}
	// 5xx<4xx 时应给出客户端归因
	if !strings.Contains(flat, "个别用户请求异常") {
		t.Errorf("insight hint missing, got: %s", flat)
	}
}

func TestBuildAlertCardSimpleFallback(t *testing.T) {
	rule := &OpsAlertRule{Name: "CPU 过高", Severity: "P2", MetricType: "cpu_usage_percent"}
	event := &OpsAlertEvent{Severity: "P2"} // 无 Breakdown
	flat := flattenAny(buildAlertCard(rule, event))
	if !strings.Contains(flat, "Description") {
		t.Errorf("simple fallback card should contain Description block, got: %s", flat)
	}
}

func TestBuildOpsAlertEmailBreakdownHTML(t *testing.T) {
	html := buildOpsAlertEmailBreakdownHTML(sampleBreakdown())
	for _, want := range []string{
		"业务上下文",
		"窗口请求 <b>81</b>",
		"田良智@云联精灵 · e77938803@gmail.com",
		"invalid_request_error ×4",
		"<li>", "</ul>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("email html missing %q\ngot:\n%s", want, html)
		}
	}
	if got := buildOpsAlertEmailBreakdownHTML(nil); got != "" {
		t.Errorf("nil breakdown should render empty html, got %q", got)
	}
}

func TestIsOpsAlertErrorRateFamily(t *testing.T) {
	cases := map[string]bool{
		"error_rate":          true,
		"success_rate":        true,
		"upstream_error_rate": true,
		"  Error_Rate ":       true,
		"cpu_usage_percent":   false,
		"":                    false,
	}
	for in, want := range cases {
		if got := isOpsAlertErrorRateFamily(in); got != want {
			t.Errorf("isOpsAlertErrorRateFamily(%q)=%v want %v", in, got, want)
		}
	}
}

// flattenAny 递归把 card 的嵌套 map/slice 拼成一个字符串,便于内容断言。
func flattenAny(v any) string {
	var b strings.Builder
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for _, val := range t {
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		case string:
			_, _ = b.WriteString(t)
			_, _ = b.WriteString("\n")
		default:
			_, _ = b.WriteString("")
		}
	}
	walk(v)
	return b.String()
}
