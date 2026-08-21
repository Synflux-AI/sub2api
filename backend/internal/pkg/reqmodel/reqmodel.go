// Package reqmodel 从已读入内存的请求体里取出客户端声明的模型名。
//
// 为什么要独立成包：这段解析原先内联在 internal/server/routes（composite 目标平台中间件），
// 而 issue #171 的认证期选组在 internal/server/middleware 里也要用它。
// routes 依赖 middleware，反向 import 会成环，所以下沉到两边都能用的位置。
//
// 本包**只解析已经读进内存的字节**，不碰 *http.Request、不负责读 body、不负责回写 body。
// 读 body 与回写必须由调用方复用同一份缓冲（body 只能读一次），
// 见 pkghttputil.ReadRequestBodyWithPrealloc。
package reqmodel

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/tidwall/gjson"
)

// jsonModelPaths 是 JSON 请求体里模型名可能出现的位置，按优先级排列。
//
// "session.model" 是 Realtime / Live 类端点的形状（POST /v1/live 等），
// 那里顶层没有 model 字段。顺序不能调：顶层 model 优先。
var jsonModelPaths = []string{"model", "session.model"}

// FromBody 依次尝试 JSON 与 multipart 两种形状，取不到返回空串。
//
// 取不到**不是错误**：大量端点没有 body 或 body 里没有模型名
// （issue #171 的调研：107 条网关路由里 54 条完全无 body）。
// 调用方应当把空串当作「这个请求无法据模型判定平台」，回退到别的信号。
func FromBody(contentType string, body []byte) string {
	if model, _ := FromJSON(body); model != "" {
		return model
	}
	return fromMultipart(contentType, body)
}

// FromJSON 返回模型名与它所在的 gjson 路径。
//
// 第二个返回值给需要**改写**模型名的调用方用（composite 路由会把客户端模型
// 改写成上游模型）。只想读的调用方用 FromBody 即可。
func FromJSON(body []byte) (string, string) {
	for _, path := range jsonModelPaths {
		model := gjson.GetBytes(body, path)
		if model.Type != gjson.String {
			continue
		}
		if value := strings.TrimSpace(model.String()); value != "" {
			return value, path
		}
	}
	return "", ""
}

// fromMultipart 处理 multipart/form-data（图片编辑、语音转写等端点）。
//
// 只看 model 字段与 session 字段（后者按 JSON 再解一层），
// 且**跳过文件部件** —— 否则上传的图片/音频会被整个读进内存。
func fromMultipart(contentType string, body []byte) string {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return ""
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ""
		}
		fieldName := part.FormName()
		if part.FileName() != "" || (fieldName != "model" && fieldName != "session") {
			continue
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return ""
		}
		switch fieldName {
		case "model":
			return strings.TrimSpace(string(data))
		case "session":
			if model, _ := FromJSON(data); model != "" {
				return model
			}
		}
	}
}
