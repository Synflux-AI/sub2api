package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 模拟一次真实上游失败：追加尝试事件，并按服务层的写法写入当前尝试的 upstream 字段。
func recordOpsTestUpstreamAttempt(c *gin.Context, accountID int64, status int, message, detail string) {
	var events []*service.OpsUpstreamErrorEvent
	if v, ok := c.Get(service.OpsUpstreamErrorsKey); ok {
		events, _ = v.([]*service.OpsUpstreamErrorEvent)
	}
	events = append(events, &service.OpsUpstreamErrorEvent{
		AccountID:          accountID,
		UpstreamStatusCode: status,
		Kind:               "failover",
		Message:            message,
		Detail:             detail,
	})
	c.Set(service.OpsUpstreamErrorsKey, events)
	service.SetOpsUpstreamError(c, status, message, detail)
}

func runOpsAttemptAttributionRequest(t *testing.T, handler gin.HandlerFunc) *service.OpsInsertErrorLogInput {
	t.Helper()
	setupOpsErrorLogTestQueue(t, 2)
	gin.SetMode(gin.TestMode)

	repo := &ingressRejectOpsRepo{}
	ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/messages", handler)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	require.Equal(t, int64(1), OpsErrorLogQueueLength())
	flushOpsErrorLogBatch([]opsErrorLogJob{<-opsErrorLogQueue})
	require.Len(t, repo.entries, 1)
	return repo.entries[0]
}

// Issue #334：A 上游 529 → B 上游 403（同号再试一次）→ C 在本机排队满被拒。
// 落库记录归属 C，不得把 B 的上游错误挂到 C 名下；完整时间线仍留在 upstream_errors。
func TestOpsErrorLoggerMiddleware_LocalRejectAfterFailoverDoesNotInheritPreviousAccountUpstream(t *testing.T) {
	const accountA, accountB, accountC = int64(101), int64(102), int64(103)

	persisted := runOpsAttemptAttributionRequest(t, func(c *gin.Context) {
		setOpsSelectedAccount(c, &service.Account{ID: accountA, Platform: service.PlatformAnthropic})
		recordOpsTestUpstreamAttempt(c, accountA, 529, "overloaded", "A body")
		setOpsSelectedAccount(c, &service.Account{ID: accountB, Platform: service.PlatformAnthropic})
		recordOpsTestUpstreamAttempt(c, accountB, http.StatusForbidden, "forbidden", "B body")
		setOpsSelectedAccount(c, &service.Account{ID: accountB, Platform: service.PlatformAnthropic})
		recordOpsTestUpstreamAttempt(c, accountB, http.StatusForbidden, "forbidden", "B body")
		setOpsSelectedAccount(c, &service.Account{ID: accountC, Platform: service.PlatformAnthropic})
		c.JSON(http.StatusTooManyRequests, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "rate_limit_error",
				"code":    gatewayQueueFullCode,
				"message": "Too many pending requests, please retry later",
			},
		})
	})

	require.Equal(t, http.StatusTooManyRequests, persisted.StatusCode)
	require.NotNil(t, persisted.AccountID)
	require.Equal(t, accountC, *persisted.AccountID)
	require.Nil(t, persisted.UpstreamStatusCode, "C 没有打上游，不能继承 B 的上游状态码")
	require.Nil(t, persisted.UpstreamErrorMessage)
	require.Nil(t, persisted.UpstreamErrorDetail)

	require.NotNil(t, persisted.UpstreamErrorsJSON)
	events, err := service.ParseOpsUpstreamErrors(*persisted.UpstreamErrorsJSON)
	require.NoError(t, err)
	require.Len(t, events, 3, "跨尝试的时间线必须完整保留")
	require.Equal(t, accountA, events[0].AccountID)
	require.Equal(t, accountB, events[2].AccountID)
	require.Equal(t, http.StatusForbidden, events[2].UpstreamStatusCode)
}

// failover 耗尽：所有号都打到上游并失败，顶层 upstream_* 仍保留最后一次上游失败的根因。
func TestOpsErrorLoggerMiddleware_FailoverExhaustedKeepsUpstreamRootCause(t *testing.T) {
	const accountA, accountB = int64(201), int64(202)

	persisted := runOpsAttemptAttributionRequest(t, func(c *gin.Context) {
		setOpsSelectedAccount(c, &service.Account{ID: accountA, Platform: service.PlatformAnthropic})
		recordOpsTestUpstreamAttempt(c, accountA, 529, "overloaded", "A body")
		setOpsSelectedAccount(c, &service.Account{ID: accountB, Platform: service.PlatformAnthropic})
		recordOpsTestUpstreamAttempt(c, accountB, http.StatusForbidden, "forbidden", "B body")
		// 选号耗尽：handleFailoverExhausted 记录原始上游状态码后映射成客户端状态码。
		service.SetOpsUpstreamError(c, http.StatusForbidden, "forbidden", "")
		c.JSON(http.StatusBadGateway, gin.H{
			"type":  "error",
			"error": gin.H{"type": "upstream_error", "message": "Upstream request failed"},
		})
	})

	require.NotNil(t, persisted.AccountID)
	require.Equal(t, accountB, *persisted.AccountID)
	require.NotNil(t, persisted.UpstreamStatusCode)
	require.Equal(t, http.StatusForbidden, *persisted.UpstreamStatusCode)
	require.NotNil(t, persisted.UpstreamErrorMessage)
	require.Equal(t, "forbidden", *persisted.UpstreamErrorMessage)
	require.NotNil(t, persisted.UpstreamErrorDetail)
	require.Equal(t, "B body", *persisted.UpstreamErrorDetail)
}

// 当前账号被选中后没有任何尝试（如准入否决），但耗尽处理在选号之后显式写入了根因：保留根因。
func TestApplyOpsUpstreamFieldsKeepsRootCauseWrittenAfterSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	setOpsSelectedAccount(c, &service.Account{ID: 302})
	recordOpsTestUpstreamAttempt(c, 302, http.StatusForbidden, "forbidden", "B body")
	setOpsSelectedAccount(c, &service.Account{ID: 303})
	service.SetOpsUpstreamError(c, http.StatusForbidden, "forbidden", "")
	entry := &service.OpsInsertErrorLogInput{}

	applyOpsUpstreamFieldsFromContext(c, entry)

	require.NotNil(t, entry.UpstreamStatusCode)
	require.Equal(t, http.StatusForbidden, *entry.UpstreamStatusCode)
	require.Len(t, entry.UpstreamErrors, 1)
}

func TestSetOpsSelectedAccountResetsAttemptUpstreamFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	setOpsSelectedAccount(c, &service.Account{ID: 401})
	recordOpsTestUpstreamAttempt(c, 401, http.StatusForbidden, "forbidden", "body")

	setOpsSelectedAccount(c, &service.Account{ID: 402})

	status, _ := c.Get(service.OpsUpstreamStatusCodeKey)
	require.Equal(t, 0, status)
	message, _ := c.Get(service.OpsUpstreamErrorMessageKey)
	require.Equal(t, "", message)
	detail, _ := c.Get(service.OpsUpstreamErrorDetailKey)
	require.Equal(t, "", detail)
	events, _ := c.Get(service.OpsUpstreamErrorsKey)
	require.Len(t, events, 1, "换号不能清掉跨尝试的时间线")
	require.True(t, hasOpsUpstreamErrorContext(c))
}
