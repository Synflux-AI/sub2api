//go:build unit

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// body 只能读一次（C9）。多分组 Key 在需要按模型选组时确实会读一次 body，
// 因此**必须**把它原样放回去 —— 否则 composite 目标平台中间件与 handler 会读到空 body，
// 表现为「请求体丢失」这种极难定位的故障。
//
// 这条测试单独成文件，因为它要的是「handler 真的把 body 读出来」的路由，
// 与回显生效分组的探针路由是两种形状。
func TestAuthMultiGroup_BodySurvivesGroupSelection(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	fx := newMultiGroupFixture(t)

	repo := &stubApiKeyRepo{
		getByKey: func(_ context.Context, key string) (*service.APIKey, error) {
			clone := *fx.apiKey
			return &clone, nil
		},
	}
	apiKeyService := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg)))
	echo := func(c *gin.Context) {
		raw, err := c.GetRawData()
		require.NoError(t, err)
		group := int64(0)
		if k, ok := GetAPIKeyFromContext(c); ok && k.Group != nil {
			group = k.Group.ID
		}
		c.JSON(http.StatusOK, gin.H{
			"body":           string(raw),
			"content_length": c.Request.ContentLength,
			"group_id":       group,
		})
	}
	router.POST("/v1/messages", echo)
	router.POST("/v1/chat/completions", echo)

	cases := []struct {
		name        string
		path        string
		payload     string
		wantGroupID int64
	}{
		{
			// 这一条最关键：选组**读过** body（要靠 model 判平台），之后 handler 仍须读到原文。
			name: "读过 body 后 handler 仍能原样读到", path: "/v1/chat/completions",
			payload: `{"model":"gpt-5.6","messages":[{"role":"user","content":"hello world"}]}`, wantGroupID: 20,
		},
		{
			name: "回退默认组的情形同样不丢 body", path: "/v1/messages",
			payload: `{"model":"unknown-model-xyz","max_tokens":128}`, wantGroupID: 10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", fx.apiKey.Key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var got map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			require.Equal(t, tc.payload, got["body"],
				"选组读过 body 之后必须原样回写，否则 handler 会读到空 body")
			require.EqualValues(t, len(tc.payload), got["content_length"],
				"ContentLength 必须与回写的 body 一致，否则下游按长度读会截断")
			require.EqualValues(t, tc.wantGroupID, got["group_id"])
		})
	}
}
