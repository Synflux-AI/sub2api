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
	description := "-"
	metricValue := "-"
	firedAt := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")

	if rule != nil {
		severity = strings.TrimSpace(rule.Severity)
		ruleName = strings.TrimSpace(rule.Name)
	}
	if event != nil {
		description = strings.TrimSpace(event.Description)
		firedAt = event.FiredAt.UTC().Format("2006-01-02 15:04:05 UTC")
		if event.MetricValue != nil {
			metricValue = fmt.Sprintf("%.4f", *event.MetricValue)
		}
	}

	headerColor := "red"
	switch strings.ToLower(severity) {
	case "warning":
		headerColor = "orange"
	case "info":
		headerColor = "blue"
	}

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": fmt.Sprintf("[Alert][%s] %s", severity, ruleName)},
			"template": headerColor,
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"fields": []any{
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Severity**\n%s", severity)},
					},
					map[string]any{
						"is_short": true,
						"text":     map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Metric Value**\n%s", metricValue)},
					},
				},
			},
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": fmt.Sprintf("**Description**\n%s", description),
				},
			},
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": fmt.Sprintf("**Fired At**\n%s", firedAt),
				},
			},
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag": "note",
				"elements": []any{
					map[string]any{"tag": "plain_text", "content": "sub2api ops alert"},
				},
			},
		},
	}
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
