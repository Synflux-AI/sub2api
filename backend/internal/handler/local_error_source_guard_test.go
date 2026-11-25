package handler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 每个 billingErrorDetails 调用点都写出网关自身生成的计费/额度/限额错误，
// 必须紧接着打本地错误标记；否则前置反向代理会把「API key 额度已用完」等
// 提示改写成笼统的上游 rate limit 文案。上游同步新增调用点时由本测试兜住。
func TestBillingErrorDetailsCallSitesMarkLocalError(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	call := regexp.MustCompile(`:= billingErrorDetails\(`)
	total := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !call.MatchString(line) {
				continue
			}
			total++
			require.Less(t, i+1, len(lines), "%s:%d", file, i+1)
			require.Equal(t, "markLocalGatewayError(c)", strings.TrimSpace(lines[i+1]),
				"%s:%d billingErrorDetails 调用后必须紧跟 markLocalGatewayError(c)", file, i+1)
		}
	}
	require.Positive(t, total)
}
