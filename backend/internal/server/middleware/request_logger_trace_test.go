package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/gin-gonic/gin"
)

func TestRequestLogger_TraceID_ValidInboundIsPropagated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.GET("/t", func(c *gin.Context) {
		got, _ := c.Request.Context().Value(ctxkey.TraceID).(string)
		if got != "trace-fixed-123" {
			t.Fatalf("trace_id in context = %q, want trace-fixed-123", got)
		}
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.Header.Set(traceid.Header, "trace-fixed-123")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get(traceid.Header); got != "trace-fixed-123" {
		t.Fatalf("response header %s = %q, want trace-fixed-123", traceid.Header, got)
	}
	for _, event := range sink.list() {
		if event == nil {
			continue
		}
		if _, ok := event.Fields["trace_id_rejected"]; ok {
			t.Fatalf("did not expect trace_id_rejected warn for valid inbound trace id")
		}
	}
}

func TestRequestLogger_TraceID_OverlongFallsBackAndWarns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.GET("/t", func(c *gin.Context) {
		requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
		traceID, _ := c.Request.Context().Value(ctxkey.TraceID).(string)
		if traceID != requestID {
			t.Fatalf("trace_id=%q, want fallback to request_id=%q", traceID, requestID)
		}
		c.Status(http.StatusOK)
	})

	overlong := strings.Repeat("a", traceid.MaxBytes+1)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.Header.Set(traceid.Header, overlong)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	found := false
	for _, event := range sink.list() {
		if event == nil || event.Message != "inbound X-Trace-Id rejected" {
			continue
		}
		found = true
		rejected, _ := event.Fields["trace_id_rejected"].(string)
		if len(rejected) != traceid.MaxBytes {
			t.Fatalf("trace_id_rejected length=%d, want %d", len(rejected), traceid.MaxBytes)
		}
	}
	if !found {
		t.Fatalf("expected warn log 'inbound X-Trace-Id rejected'")
	}
}

func TestRequestLogger_TraceID_MissingFallsBackWithoutWarn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.GET("/t", func(c *gin.Context) {
		requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
		traceID, _ := c.Request.Context().Value(ctxkey.TraceID).(string)
		if traceID != requestID {
			t.Fatalf("trace_id=%q, want fallback to request_id=%q", traceID, requestID)
		}
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Header().Get(traceid.Header) == "" {
		t.Fatalf("response header %s should be set", traceid.Header)
	}
	for _, event := range sink.list() {
		if event == nil {
			continue
		}
		if event.Message == "inbound X-Trace-Id rejected" {
			t.Fatalf("did not expect warn when trace id is missing")
		}
	}
}

func TestRequestLogger_TraceID_TrimsSurroundingWhitespace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initMiddlewareTestLogger(t)

	r := gin.New()
	r.Use(RequestLogger())
	r.GET("/t", func(c *gin.Context) {
		got, _ := c.Request.Context().Value(ctxkey.TraceID).(string)
		if got != "trace-trim-me" {
			t.Fatalf("trace_id=%q, want trace-trim-me", got)
		}
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.Header.Set(traceid.Header, "  trace-trim-me  ")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get(traceid.Header); got != "trace-trim-me" {
		t.Fatalf("response header=%q, want trace-trim-me", got)
	}
}

func TestRequestLogger_TraceID_ResponseHeaderMatchesContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initMiddlewareTestLogger(t)

	var ctxTraceID string
	r := gin.New()
	r.Use(RequestLogger())
	r.GET("/t", func(c *gin.Context) {
		ctxTraceID, _ = c.Request.Context().Value(ctxkey.TraceID).(string)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	headerTraceID := w.Header().Get(traceid.Header)
	if headerTraceID == "" {
		t.Fatalf("response header %s should not be empty", traceid.Header)
	}
	if headerTraceID != ctxTraceID {
		t.Fatalf("response header=%q, context=%q, want match", headerTraceID, ctxTraceID)
	}
}
