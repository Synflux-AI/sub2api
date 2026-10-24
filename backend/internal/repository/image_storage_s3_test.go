//go:build unit

package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestS3ImageStorage_SaveCacheControl(t *testing.T) {
	var mu sync.Mutex
	var cacheControl, contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cacheControl = r.Header.Get("Cache-Control")
		contentType = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	newStorage := func(publicBaseURL string) *S3ImageStorage {
		storage, err := NewS3ImageStorage(context.Background(), &config.ImageStorageConfig{
			Endpoint: server.URL, Region: "auto", Bucket: "img-bucket",
			AccessKeyID: "test-ak", SecretAccessKey: "test-sk", ForcePathStyle: true,
			PublicBaseURL: publicBaseURL,
		})
		require.NoError(t, err)
		return storage
	}

	url, err := newStorage("https://img.example.com/").Save(context.Background(), "images/a.png", "image/png", []byte("png"))
	require.NoError(t, err)
	require.Equal(t, "https://img.example.com/images/a.png", url)
	require.Equal(t, imageStoragePublicCacheControl, cacheControl)
	require.Equal(t, "image/png", contentType)

	url, err = newStorage("").Save(context.Background(), "images/b.png", "image/png", []byte("png"))
	require.NoError(t, err)
	require.True(t, strings.Contains(url, "X-Amz-Signature="), "未配 public_base_url 时仍返回 presigned（异步任务沿用）")
	require.Empty(t, cacheControl, "presigned 对象不加公开缓存头")
}
