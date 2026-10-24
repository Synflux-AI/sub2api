package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	// openAIImageURLRewriteTimeout 是一次响应改写（全部图片的下载 + 上传）的整体上限。
	openAIImageURLRewriteTimeout = 120 * time.Second
	// openAIImageURLUploadTimeout 是单张图片上传对象存储的超时上限。
	openAIImageURLUploadTimeout = 60 * time.Second
	// openAIImageURLRewriteRequestConcurrency 是单个响应内并行下载的图片数。
	openAIImageURLRewriteRequestConcurrency = 4
	// openAIImageURLRewriteProcessConcurrency 是全进程同时进行的下载数：
	// 高峰时防止每个请求各占 4×max_download_bytes 叠加撑爆内存。
	openAIImageURLRewriteProcessConcurrency = 16
	// openAIImageURLInlineBudgetBytes 是单个响应内联下载图片的总字节预算，超出的图片保留上游 url。
	openAIImageURLInlineBudgetBytes int64 = 64 << 20
)

// ErrOpenAIImagesURLRewriteIncomplete 表示 url_rewrite_strict 开启时仍有图片只能返回上游 url。
// 它不是 failover 错误：上游已出图并计费，换号重试会重复生成；由 handler 写 502 兜底且不计费。
var ErrOpenAIImagesURLRewriteIncomplete = errors.New("image url rewrite incomplete: upstream image url could not be hidden")

// openAIImageURLRewriteSlots 是进程级下载信号量，见 openAIImageURLRewriteProcessConcurrency。
var openAIImageURLRewriteSlots = make(chan struct{}, openAIImageURLRewriteProcessConcurrency)

type imageURLRewriteSkippedKey struct{}

// WithImageURLRewriteSkipped 标记该请求跳过 image_storage.url_mode 改写。
// 异步生图任务完成时由 ImageTaskService 统一转存对象存储，同步入口再改写会重复下载/上传，
// inline 模式还会先拼出 data URL 再被解码，白占一份内存。
func WithImageURLRewriteSkipped(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, imageURLRewriteSkippedKey{}, true)
}

// ImageURLRewriteSkipped 报告 ctx 是否带有 WithImageURLRewriteSkipped 标记。
func ImageURLRewriteSkipped(ctx context.Context) bool {
	return ctx != nil && ctx.Value(imageURLRewriteSkippedKey{}) == true
}

// SetImageURLPolicyResolver 注入 image_storage.url_mode 解析函数；未注入时等同 passthrough。
func (s *OpenAIGatewayService) SetImageURLPolicyResolver(resolver ImageURLPolicyResolver) {
	if s == nil {
		return
	}
	s.imageURLPolicy.Store(&resolver)
}

// activeImageURLPolicy 返回本次响应要执行的改写策略；ok=false 表示保持现状（passthrough）。
func (s *OpenAIGatewayService) activeImageURLPolicy(ctx context.Context, c *gin.Context) (ImageURLPolicy, bool) {
	if s == nil || ImageURLRewriteSkipped(ctx) {
		return ImageURLPolicy{}, false
	}
	if c != nil && c.Request != nil && ImageURLRewriteSkipped(c.Request.Context()) {
		return ImageURLPolicy{}, false
	}
	resolver := s.imageURLPolicy.Load()
	if resolver == nil || *resolver == nil {
		return ImageURLPolicy{}, false
	}
	policy := (*resolver)()
	switch policy.Mode {
	case ImageURLModeInline, ImageURLModeRehost:
		if policy.MaxDownloadBytes <= 0 {
			policy.MaxDownloadBytes = defaultImageMaxDownloadBytes
		}
		return policy, true
	default:
		return ImageURLPolicy{}, false
	}
}

// validateImageMediaURL 校验上游生图响应里的图片 url。
// 不使用 security.url_allowlist.upstream_hosts（图片 CDN 通常不在其中，否则会一直回退泄露），
// 只在配置了 media_hosts 时按其限定域名；私网/回环字面量一律拒绝，解析结果与重定向
// 由 downloadOpenAIImageURL 的 WithHTTPUpstreamPublicHostsOnly 逐跳校验。
func (s *OpenAIGatewayService) validateImageMediaURL(raw string) (string, error) {
	allowInsecureHTTP := true
	var mediaHosts []string
	if s.cfg != nil {
		allowlist := s.cfg.Security.URLAllowlist
		allowInsecureHTTP = !allowlist.Enabled && allowlist.AllowInsecureHTTP
		mediaHosts = allowlist.MediaHosts
	}
	trimmed := strings.TrimSpace(raw)
	// 只用校验结果判定合法性，下载仍用原始 url：规范化会改写已编码路径，破坏 CDN 签名。
	if _, err := urlvalidator.ValidateHTTPURL(trimmed, allowInsecureHTTP, urlvalidator.ValidationOptions{
		AllowedHosts: mediaHosts,
	}); err != nil {
		return "", err
	}
	return trimmed, nil
}

// openAIImageURLRewriteOp 是对 data[index] 的一次改写；各字段为空表示不动。
type openAIImageURLRewriteOp struct {
	index     int
	setURL    string
	setB64    string
	deleteURL bool
	deleteB64 bool
}

type openAIImageURLRewriteTask struct {
	index  int
	rawURL string
}

// rewriteOpenAIImagesResponseURLs 按 image_storage.url_mode 改写非流式 Images 响应里的上游图片 url。
//
// inline：url 换成 data:image/...;base64,...；客户端要 b64（显式 b64_json，或未指定且账号开了
// images_url_to_b64_json 回填）时改写为 b64_json。rehost：下载转存对象存储，url 换成自己的直链。
// 尽力隐藏：下载失败保留上游 url；rehost 上传失败退化为 data URL。任何失败都不删除唯一的图片字段，
// 否则会漏计费。返回值 changed 报告 body 是否被改写，exposed 是仍保留上游 http(s) url 的图片数
// （url_rewrite_strict 据此决定是否改回 502）。
func (s *OpenAIGatewayService) rewriteOpenAIImagesResponseURLs(
	ctx context.Context,
	account *Account,
	parsed *OpenAIImagesRequest,
	policy ImageURLPolicy,
	body []byte,
) (out []byte, changed bool, exposed int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false, 0
	}
	items := gjson.GetBytes(body, "data")
	if !items.IsArray() {
		return body, false, 0
	}
	responseFormat := ""
	if parsed != nil {
		responseFormat = parsed.ResponseFormat
	}
	explicitURL := responseFormat == "url"
	preferB64 := responseFormat == "b64_json" || (responseFormat == "" && ImagesURLToB64JSONEnabled(account))
	rehost := policy.Mode == ImageURLModeRehost && policy.Storage != nil

	ctx, cancel := context.WithTimeout(ctx, openAIImageURLRewriteTimeout)
	defer cancel()

	var ops []openAIImageURLRewriteOp
	var tasks []openAIImageURLRewriteTask
	for index, item := range items.Array() {
		if !item.IsObject() {
			continue
		}
		rawURL := strings.TrimSpace(item.Get("url").String())
		if !isHTTPImageURL(rawURL) {
			// 没有 url、或已是 data: URL（不暴露任何域名）时不动。
			continue
		}
		b64 := strings.TrimSpace(item.Get("b64_json").String())
		if b64 == "" {
			tasks = append(tasks, openAIImageURLRewriteTask{index: index, rawURL: rawURL})
			continue
		}
		// 上游同时给了 b64_json 与 url：图片字节已在手，无需下载。
		if !explicitURL {
			ops = append(ops, openAIImageURLRewriteOp{index: index, deleteURL: true})
			continue
		}
		op := openAIImageURLRewriteOp{index: index, deleteB64: true}
		op.setURL = s.rehostOrInlineB64(ctx, account, policy, rehost, index, b64)
		ops = append(ops, op)
	}

	if len(tasks) > 0 {
		taskOps := s.runOpenAIImageURLRewriteTasks(ctx, account, policy, rehost, preferB64, tasks)
		exposed += len(tasks) - len(taskOps)
		ops = append(ops, taskOps...)
	}

	// 每个 op 都对应一张带上游 url 的图片：写回失败即仍然暴露。
	for _, op := range ops {
		updated, ok := applyOpenAIImageURLRewriteOp(body, op)
		if !ok {
			exposed++
			logger.L().Warn("openai.images.url_rewrite_apply_failed",
				zap.Int64("account_id", accountIDOf(account)), zap.Int("index", op.index))
			continue
		}
		body = updated
		changed = true
	}
	return body, changed, exposed
}

// runOpenAIImageURLRewriteTasks 并行下载 url-only 的图片项，结果按 tasks 顺序返回。
func (s *OpenAIGatewayService) runOpenAIImageURLRewriteTasks(
	ctx context.Context,
	account *Account,
	policy ImageURLPolicy,
	rehost bool,
	preferB64 bool,
	tasks []openAIImageURLRewriteTask,
) []openAIImageURLRewriteOp {
	results := make([]*openAIImageURLRewriteOp, len(tasks))
	var inlineBytes atomic.Int64
	requestSlots := make(chan struct{}, openAIImageURLRewriteRequestConcurrency)
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task openAIImageURLRewriteTask) {
			defer wg.Done()
			requestSlots <- struct{}{}
			defer func() { <-requestSlots }()
			results[i] = s.rewriteOpenAIImageURLItem(ctx, account, policy, rehost, preferB64, task, &inlineBytes)
		}(i, task)
	}
	wg.Wait()

	ops := make([]openAIImageURLRewriteOp, 0, len(results))
	for _, op := range results {
		if op != nil {
			ops = append(ops, *op)
		}
	}
	return ops
}

// rewriteOpenAIImageURLItem 下载一张 url-only 图片并决定改写方式；返回 nil 表示保留上游 url。
func (s *OpenAIGatewayService) rewriteOpenAIImageURLItem(
	ctx context.Context,
	account *Account,
	policy ImageURLPolicy,
	rehost bool,
	preferB64 bool,
	task openAIImageURLRewriteTask,
	inlineBytes *atomic.Int64,
) *openAIImageURLRewriteOp {
	data, contentType, err := s.fetchOpenAIImageMediaURL(ctx, account, task.rawURL, policy.MaxDownloadBytes)
	if err != nil {
		logOpenAIImageURLRewriteFailure(account, policy.Mode, task.index, "download", err)
		return nil
	}
	// 只有结果会以 base64 留在响应里时才计入内联预算；rehost 成功后只剩短链接。
	inlined := preferB64 || !rehost
	if inlined && inlineBytes.Add(int64(len(data))) > openAIImageURLInlineBudgetBytes {
		logOpenAIImageURLRewriteFailure(account, policy.Mode, task.index, "inline_budget",
			fmt.Errorf("inline image bytes exceed %d", openAIImageURLInlineBudgetBytes))
		return nil
	}
	if preferB64 {
		return &openAIImageURLRewriteOp{index: task.index, setB64: base64.StdEncoding.EncodeToString(data), deleteURL: true}
	}
	op := &openAIImageURLRewriteOp{index: task.index, deleteB64: true}
	if rehost {
		if hosted, err := s.saveOpenAIImageToStorage(ctx, policy, contentType, data); err == nil {
			op.setURL = hosted
			return op
		} else {
			logOpenAIImageURLRewriteFailure(account, policy.Mode, task.index, "upload", err)
		}
	}
	op.setURL = imageDataURL(contentType, base64.StdEncoding.EncodeToString(data))
	return op
}

// rehostOrInlineB64 把上游已给出的 b64_json 转成要写回 url 字段的值：
// rehost 时上传并返回直链，否则（或上传失败）返回 data URL。
func (s *OpenAIGatewayService) rehostOrInlineB64(
	ctx context.Context,
	account *Account,
	policy ImageURLPolicy,
	rehost bool,
	index int,
	b64 string,
) string {
	contentType := sniffBase64ImageContentType(b64)
	if rehost {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err == nil && int64(len(data)) <= policy.MaxDownloadBytes {
			if contentType = detectedImageContentType(data); contentType == "" {
				contentType = "image/png"
			}
			hosted, uploadErr := s.saveOpenAIImageToStorage(ctx, policy, contentType, data)
			if uploadErr == nil {
				return hosted
			}
			err = uploadErr
		} else if err == nil {
			err = fmt.Errorf("decoded image exceeds %d bytes", policy.MaxDownloadBytes)
		}
		logOpenAIImageURLRewriteFailure(account, policy.Mode, index, "upload", err)
	}
	return imageDataURL(contentType, b64)
}

// fetchOpenAIImageMediaURL 经进程级信号量限流后安全下载上游图片 url。
func (s *OpenAIGatewayService) fetchOpenAIImageMediaURL(ctx context.Context, account *Account, rawURL string, limit int64) ([]byte, string, error) {
	downloadURL, err := s.validateImageMediaURL(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid image url: %w", err)
	}
	select {
	case openAIImageURLRewriteSlots <- struct{}{}:
		defer func() { <-openAIImageURLRewriteSlots }()
	case <-ctx.Done():
		return nil, "", fmt.Errorf("wait for image download slot: %w", ctx.Err())
	}
	return s.downloadOpenAIImageURL(ctx, account, downloadURL, limit)
}

// saveOpenAIImageToStorage 以随机 UUID key（沿用 image_storage.prefix）上传图片并返回直链。
func (s *OpenAIGatewayService) saveOpenAIImageToStorage(ctx context.Context, policy ImageURLPolicy, contentType string, data []byte) (string, error) {
	if policy.Storage == nil {
		return "", errors.New("image storage is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, openAIImageURLUploadTimeout)
	defer cancel()
	key := policy.Prefix + uuid.NewString() + extensionForContentType(contentType)
	hosted, err := policy.Storage.Save(ctx, key, contentType, data)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(hosted) == "" {
		return "", errors.New("image storage returned an empty url")
	}
	return hosted, nil
}

func applyOpenAIImageURLRewriteOp(body []byte, op openAIImageURLRewriteOp) ([]byte, bool) {
	prefix := fmt.Sprintf("data.%d.", op.index)
	var err error
	// 先写新值再删旧字段：任一步失败都返回原 body，保证图片字段不会被删空。
	updated := body
	if op.setURL != "" {
		if updated, err = sjson.SetBytes(updated, prefix+"url", op.setURL); err != nil {
			return body, false
		}
	}
	if op.setB64 != "" {
		if updated, err = sjson.SetBytes(updated, prefix+"b64_json", op.setB64); err != nil {
			return body, false
		}
	}
	if op.deleteURL {
		if updated, err = sjson.DeleteBytes(updated, prefix+"url"); err != nil {
			return body, false
		}
	}
	if op.deleteB64 {
		if updated, err = sjson.DeleteBytes(updated, prefix+"b64_json"); err != nil {
			return body, false
		}
	}
	return updated, true
}

// stripRewrittenBodyValidators 删除改写后已失效的校验头，避免客户端/CDN 按旧值缓存或校验；
// Content-Length 由 c.Data 按新 body 重算。
func stripRewrittenBodyValidators(header http.Header) {
	header.Del("ETag")
	header.Del("Content-MD5")
	header.Del("Content-Length")
}

func isHTTPImageURL(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

func imageDataURL(contentType, b64 string) string {
	if contentType == "" {
		contentType = "image/png"
	}
	return "data:" + contentType + ";base64," + b64
}

// sniffBase64ImageContentType 只解码 base64 前缀嗅探 MIME，避免为拼 data URL 解码整张图。
func sniffBase64ImageContentType(b64 string) string {
	const prefixChars = 684 // 4 的倍数，解码后 ≥512 字节，覆盖 http.DetectContentType 的嗅探窗口
	prefix := b64
	if len(prefix) > prefixChars {
		prefix = prefix[:prefixChars]
	}
	data, err := base64.StdEncoding.DecodeString(prefix)
	if err != nil {
		return "image/png"
	}
	return detectImageContentType(data)
}

func logOpenAIImageURLRewriteFailure(account *Account, mode string, index int, stage string, err error) {
	logger.L().Warn("openai.images.url_rewrite_failed",
		zap.Int64("account_id", accountIDOf(account)),
		zap.String("url_mode", mode),
		zap.String("stage", stage),
		zap.Int("index", index),
		zap.String("error", sanitizeUpstreamErrorMessage(err.Error())),
	)
}

func accountIDOf(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}
