package httputil

import "net/http"

// ErrorSourceHeader 标记错误响应由本网关自身生成（API Key 额度/限额、
// 用户/分组 RPM、用户并发等），而不是转发自上游账号。
//
// 前置反向代理可据此只放行本地生成的错误体，继续改写上游原样错误，避免把
// 「API key 额度已用完」这类可操作的提示改写成笼统的 rate limit 文案。
// 反向代理应在返回给最终客户端前剥掉该头。
const (
	ErrorSourceHeader = "X-Sub2API-Error-Source"
	ErrorSourceLocal  = "local"
)

// MarkLocalError 在响应头上标记本地生成的错误。只能在写出本网关自身生成
// 的错误体之前调用，不能用于转发上游响应体的路径。
func MarkLocalError(h http.Header) {
	if h == nil {
		return
	}
	h.Set(ErrorSourceHeader, ErrorSourceLocal)
}
