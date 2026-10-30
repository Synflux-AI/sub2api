package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeOpsAccountConcurrencyReader struct {
	inflight   map[int64]int
	waiting    int
	inflightEr error
	calls      int
	ctxAlive   bool
}

func (r *fakeOpsAccountConcurrencyReader) GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	r.calls++
	r.ctxAlive = ctx.Err() == nil
	if r.inflightEr != nil {
		return nil, r.inflightEr
	}
	out := make(map[int64]int, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = r.inflight[id]
	}
	return out, nil
}

func (r *fakeOpsAccountConcurrencyReader) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return r.waiting, nil
}

const testOpenAIOverloadDetail = `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"sequence_number":3,"type":"error"}`

func newOverloadSnapshotTestContext(t *testing.T, reader OpsAccountConcurrencyReader, account *Account) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	SetOpsAccountConcurrencyReader(c, reader)
	SetOpsSelectedAccountConcurrency(c, account)
	return c
}

func lastOverloadSnapshotEvent(t *testing.T, c *gin.Context) *OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.NotEmpty(t, events)
	return events[len(events)-1]
}

func TestAppendOpsUpstreamError_OpenAIOverloadRecordsConcurrencySnapshot(t *testing.T) {
	loadFactor := 8
	reader := &fakeOpsAccountConcurrencyReader{inflight: map[int64]int{55: 0}, waiting: 2}
	c := newOverloadSnapshotTestContext(t, reader, &Account{ID: 55, Concurrency: 10, LoadFactor: &loadFactor})
	// 请求已取消时仍要读到 Redis 计数。
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           PlatformOpenAI,
		AccountID:          55,
		UpstreamStatusCode: 503,
		Kind:               "stream_failed",
		Message:            "Our servers are currently overloaded. Please try again later.",
		Detail:             testOpenAIOverloadDetail,
	})

	ev := lastOverloadSnapshotEvent(t, c)
	require.NotNil(t, ev.AccountConcurrency)
	require.Equal(t, 10, *ev.AccountConcurrency)
	require.NotNil(t, ev.AccountLoadFactor)
	require.Equal(t, 8, *ev.AccountLoadFactor)
	require.NotNil(t, ev.AccountInflight)
	require.Equal(t, 0, *ev.AccountInflight)
	require.NotNil(t, ev.AccountWaiting)
	require.Equal(t, 2, *ev.AccountWaiting)
	require.True(t, reader.ctxAlive)

	// 在途为 0 也必须落进 JSON，否则无法区分「没有在途」与「没采到」。
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"account_inflight":0`)
	require.Contains(t, string(raw), `"account_concurrency":10`)
}

func TestAppendOpsUpstreamError_OverloadDetectedFromDetailCode(t *testing.T) {
	reader := &fakeOpsAccountConcurrencyReader{inflight: map[int64]int{89: 3}}
	c := newOverloadSnapshotTestContext(t, reader, &Account{ID: 89, Concurrency: 5})

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:  PlatformOpenAI,
		AccountID: 89,
		Kind:      "failover",
		Detail:    `{"error":{"code":"server_is_overloaded"}}`,
	})

	ev := lastOverloadSnapshotEvent(t, c)
	require.NotNil(t, ev.AccountInflight)
	require.Equal(t, 3, *ev.AccountInflight)
	require.Nil(t, ev.AccountLoadFactor)
}

func TestAppendOpsUpstreamError_SnapshotSkipsNonOverloadAndOtherPlatforms(t *testing.T) {
	cases := []OpsUpstreamErrorEvent{
		{Platform: PlatformOpenAI, AccountID: 55, UpstreamStatusCode: 429, Message: "Rate limit reached"},
		{Platform: PlatformAnthropic, AccountID: 55, UpstreamStatusCode: 529, Message: "Our servers are currently overloaded."},
	}
	for _, in := range cases {
		reader := &fakeOpsAccountConcurrencyReader{inflight: map[int64]int{55: 1}}
		c := newOverloadSnapshotTestContext(t, reader, &Account{ID: 55, Concurrency: 10})

		appendOpsUpstreamError(c, in)

		ev := lastOverloadSnapshotEvent(t, c)
		require.Nil(t, ev.AccountConcurrency)
		require.Nil(t, ev.AccountInflight)
		require.Zero(t, reader.calls)
	}
}

func TestAppendOpsUpstreamError_SnapshotOmitsMismatchedOrUnavailableValues(t *testing.T) {
	reader := &fakeOpsAccountConcurrencyReader{inflightEr: errors.New("redis down")}
	// 选中账号与失败账号不一致时，不把别的账号的上限记到这次失败上。
	c := newOverloadSnapshotTestContext(t, reader, &Account{ID: 31, Concurrency: 5})

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:  PlatformOpenAI,
		AccountID: 105,
		Message:   "Our servers are currently overloaded. Please try again later.",
	})

	ev := lastOverloadSnapshotEvent(t, c)
	require.Nil(t, ev.AccountConcurrency)
	require.Nil(t, ev.AccountInflight)
	require.NotNil(t, ev.AccountWaiting)
}

func TestAppendOpsUpstreamError_SnapshotWithoutReaderKeepsConfiguredCap(t *testing.T) {
	c := newOverloadSnapshotTestContext(t, nil, &Account{ID: 55, Concurrency: 10})

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:  PlatformOpenAI,
		AccountID: 55,
		Message:   "Our servers are currently overloaded. Please try again later.",
	})

	ev := lastOverloadSnapshotEvent(t, c)
	require.NotNil(t, ev.AccountConcurrency)
	require.Equal(t, 10, *ev.AccountConcurrency)
	require.Nil(t, ev.AccountInflight)
}

func TestOpsServiceAccountConcurrencyReader_NilSafe(t *testing.T) {
	var nilSvc *OpsService
	require.Nil(t, nilSvc.AccountConcurrencyReader())
	require.Nil(t, (&OpsService{}).AccountConcurrencyReader())
}
