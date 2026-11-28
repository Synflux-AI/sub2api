package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// Anthropic 粘性会话「定深锚点」（#347）。
//
// 客户端不带 metadata.user_id 时，原 GenerateSessionHash 的第 2、3 档 key 在同一会话内
// 并不稳定（cache_control 每轮挪到最新消息），同一会话会被拆到多个上游账号而冷写缓存。
// 定深锚点只用对话开头的前 K 条消息做会话标识：
//
//	H_i = hash(system + 前 i 条消息)，i = 1 … min(n, K)
//
// 查找时一次批量读取 H_min(n,K) … H_1，从深到浅取第一个命中；转发成功后只写最深的
// H_min(n,K)。对话超过 K 条消息后标识固定，Redis 占用只随活跃会话数增长。
//
// 锚点 key 复用粘性会话存储（sticky_session:{groupID}:{sessionHash}），sessionHash 带独立
// 前缀 "anchor:"，与 metadata session id / 内容 hash 两类既有 key 互不覆盖；调度层对粘性
// key 的清理、选号时绑定、max_sessions 计数因此对锚点原样生效。

const (
	defaultAnthropicStickyAnchorDepth = 3
	minAnthropicStickyAnchorDepth     = 1
	maxAnthropicStickyAnchorDepth     = 8

	// anthropicStickyAnchorKeyPrefix 锚点 sessionHash 前缀，与既有粘性 key 隔离。
	anthropicStickyAnchorKeyPrefix = "anchor:"
	// anthropicStickyAnchorHashBytes 锚点 hash 长度（128 位）。
	anthropicStickyAnchorHashBytes = 16
)

// AnthropicStickyAnchorSettings 定深锚点运行时设置。
type AnthropicStickyAnchorSettings struct {
	Enabled bool
	Depth   int
}

func defaultAnthropicStickyAnchorSettings() AnthropicStickyAnchorSettings {
	return AnthropicStickyAnchorSettings{Enabled: true, Depth: defaultAnthropicStickyAnchorDepth}
}

// normalizeAnthropicStickyAnchorDepth 越界按默认深度处理。
func normalizeAnthropicStickyAnchorDepth(depth int) int {
	if depth < minAnthropicStickyAnchorDepth || depth > maxAnthropicStickyAnchorDepth {
		return defaultAnthropicStickyAnchorDepth
	}
	return depth
}

// parseAnthropicStickyAnchorSettings 从设置键值解析：缺失/空值的开关按开启处理，
// 深度解析失败或越界按 3 处理。
func parseAnthropicStickyAnchorSettings(values map[string]string) AnthropicStickyAnchorSettings {
	result := defaultAnthropicStickyAnchorSettings()
	if v, ok := values[SettingKeyAnthropicStickyAnchorEnabled]; ok && v != "" {
		result.Enabled = v == "true"
	}
	if v := strings.TrimSpace(values[SettingKeyAnthropicStickyAnchorDepth]); v != "" {
		if depth, err := strconv.Atoi(v); err == nil {
			result.Depth = normalizeAnthropicStickyAnchorDepth(depth)
		}
	}
	return result
}

// AnthropicStickyResolution 是 /v1/messages 入口按定深锚点解析出的粘性会话。
type AnthropicStickyResolution struct {
	// SessionHash 交给调度层的会话标识（最深锚点，带 "anchor:" 前缀）。
	SessionHash string
	// BoundAccountID 从深到浅第一个命中的锚点绑定的账号；0 表示新会话。
	BoundAccountID int64
	// ConfiguredDepth 配置深度 K；AnchorDepth 本次最深锚点 min(n, K)；
	// MatchedDepth 命中锚点的深度，0 表示未命中。
	ConfiguredDepth int
	AnchorDepth     int
	MatchedDepth    int
}

// stickySessionBatchReader 由支持批量读取的粘性缓存实现（Redis MGET）。
// 返回值与 sessionHashes 一一对应，未绑定为 0。
type stickySessionBatchReader interface {
	GetSessionAccountIDs(ctx context.Context, groupID int64, sessionHashes []string) ([]int64, error)
}

// ResolveAnthropicStickyAnchor 按定深锚点解析 /v1/messages 请求的粘性会话。
//
// ok=false 表示本次不走锚点，调用方应按原 GenerateSessionHash 取 key：
// 请求带 metadata.user_id 会话、开关关闭、没有消息、缓存不可用或读取失败。
func (s *GatewayService) ResolveAnthropicStickyAnchor(ctx context.Context, groupID *int64, parsed *ParsedRequest) (AnthropicStickyResolution, bool) {
	if s == nil || parsed == nil || s.cache == nil {
		return AnthropicStickyResolution{}, false
	}
	// metadata.user_id 仍为第一优先级，行为不变。
	if parsed.MetadataUserID != "" {
		if uid := ParseMetadataUserID(parsed.MetadataUserID); uid != nil && uid.SessionID != "" {
			return AnthropicStickyResolution{}, false
		}
	}
	settings := defaultAnthropicStickyAnchorSettings()
	if s.settingService != nil {
		settings = s.settingService.GetAnthropicStickyAnchorSettings(ctx)
	}
	if !settings.Enabled {
		return AnthropicStickyResolution{}, false
	}
	depth := normalizeAnthropicStickyAnchorDepth(settings.Depth)
	anchors := buildAnthropicStickyAnchors(parsed.SystemRaw(), parsed.MessagesRaw(), depth)
	if len(anchors) == 0 {
		return AnthropicStickyResolution{}, false
	}

	// 从深到浅：keys[0] 是最深锚点。
	keys := make([]string, len(anchors))
	for i := range anchors {
		keys[i] = anchors[len(anchors)-1-i]
	}
	accountIDs, err := s.getStickySessionAccountIDs(ctx, derefGroupID(groupID), keys)
	if err != nil {
		slog.Warn("sticky.anchor_lookup_failed",
			"group_id", derefGroupID(groupID),
			"anchor_depth", len(anchors),
			"error", err,
		)
		return AnthropicStickyResolution{}, false
	}

	res := AnthropicStickyResolution{
		SessionHash:     keys[0],
		ConfiguredDepth: depth,
		AnchorDepth:     len(anchors),
	}
	for i, accountID := range accountIDs {
		if accountID > 0 {
			res.BoundAccountID = accountID
			res.MatchedDepth = len(anchors) - i
			break
		}
	}
	slog.Info("sticky.hash_source",
		"source", "anchor",
		"hash", res.SessionHash,
		"depth", res.ConfiguredDepth,
		"anchor_depth", res.AnchorDepth,
		"matched_depth", res.MatchedDepth,
		"new_session", res.MatchedDepth == 0,
	)
	return res, true
}

func (s *GatewayService) getStickySessionAccountIDs(ctx context.Context, groupID int64, sessionHashes []string) ([]int64, error) {
	if reader, ok := s.cache.(stickySessionBatchReader); ok {
		ids, err := reader.GetSessionAccountIDs(ctx, groupID, sessionHashes)
		if err != nil {
			return nil, err
		}
		if len(ids) != len(sessionHashes) {
			return nil, errors.New("sticky session batch read returned mismatched length")
		}
		return ids, nil
	}
	ids := make([]int64, len(sessionHashes))
	for i, sessionHash := range sessionHashes {
		accountID, err := s.cache.GetSessionAccountID(ctx, groupID, sessionHash)
		if err != nil {
			if errors.Is(err, ErrStickySessionNotFound) {
				continue
			}
			return nil, err
		}
		ids[i] = accountID
	}
	return ids, nil
}

// buildAnthropicStickyAnchors 返回 H_1 … H_min(n,K)（带 "anchor:" 前缀，浅到深）。
// 只处理前 K 条消息，不扫描整个请求体；没有消息时返回 nil。
func buildAnthropicStickyAnchors(systemRaw, messagesRaw []byte, depth int) []string {
	depth = normalizeAnthropicStickyAnchorDepth(depth)
	messages := parseRawJSONView(messagesRaw)
	if !messages.IsArray() {
		return nil
	}

	h := sha256.New()
	w := anchorHashWriter{h: h}
	w.tag('S')
	w.canonicalContent(parseRawJSONView(systemRaw), false)

	anchors := make([]string, 0, depth)
	var sum [sha256.Size]byte
	messages.ForEach(func(_, msg gjson.Result) bool {
		if !msg.IsObject() {
			// 非对象消息无法规范化，后续锚点不再可靠，停在已有深度。
			return false
		}
		role := msg.Get("role").String()
		w.tag('M')
		w.str(role)
		w.canonicalContent(msg.Get("content"), role == "assistant")
		digest := h.Sum(sum[:0])
		anchors = append(anchors, anthropicStickyAnchorKeyPrefix+hex.EncodeToString(digest[:anthropicStickyAnchorHashBytes]))
		return len(anchors) < depth
	})
	if len(anchors) == 0 {
		return nil
	}
	return anchors
}

// anchorHashWriter 把 JSON 值以带类型标签与长度前缀的规范形式写入 hash：
// 对象键排序、任意深度剥离 cache_control、字符串按解码后的值写入。
type anchorHashWriter struct {
	h   hash.Hash
	buf [9]byte
}

func (w *anchorHashWriter) tag(b byte) {
	w.buf[0] = b
	_, _ = w.h.Write(w.buf[:1])
}

func (w *anchorHashWriter) str(s string) {
	w.buf[0] = 's'
	binary.BigEndian.PutUint64(w.buf[1:9], uint64(len(s)))
	_, _ = w.h.Write(w.buf[:9])
	if len(s) > 0 {
		// 只读写入 hash，避免为大字符串（如 base64 图片）复制一份。
		_, _ = w.h.Write(unsafe.Slice(unsafe.StringData(s), len(s)))
	}
}

// canonicalContent 规范化 system 或消息 content：字符串等价于单个 text 块，
// 数组逐块规范化；dropThinking 时跳过 thinking / redacted_thinking 块。
func (w *anchorHashWriter) canonicalContent(content gjson.Result, dropThinking bool) {
	switch {
	case !content.Exists() || content.Type == gjson.Null:
		w.tag('0')
	case content.Type == gjson.String:
		w.tag('[')
		w.textBlock(content.Str)
		w.tag(']')
	case content.IsArray():
		w.tag('[')
		content.ForEach(func(_, block gjson.Result) bool {
			if dropThinking && block.IsObject() {
				switch block.Get("type").String() {
				case "thinking", "redacted_thinking":
					return true
				}
			}
			w.value(block)
			return true
		})
		w.tag(']')
	default:
		w.value(content)
	}
}

// textBlock 写入与 {"type":"text","text":s} 规范化结果相同的字节。
func (w *anchorHashWriter) textBlock(s string) {
	w.tag('{')
	w.str("text")
	w.str(s)
	w.str("type")
	w.str("text")
	w.tag('}')
}

func (w *anchorHashWriter) value(v gjson.Result) {
	switch v.Type {
	case gjson.String:
		w.str(v.Str)
	case gjson.Number:
		w.tag('n')
		w.str(v.Raw)
	case gjson.True:
		w.tag('t')
	case gjson.False:
		w.tag('f')
	case gjson.Null:
		w.tag('0')
	case gjson.JSON:
		if v.IsArray() {
			w.tag('[')
			v.ForEach(func(_, item gjson.Result) bool {
				w.value(item)
				return true
			})
			w.tag(']')
			return
		}
		type kv struct {
			key string
			val gjson.Result
		}
		var fields []kv
		v.ForEach(func(key, val gjson.Result) bool {
			if key.Str != "cache_control" {
				fields = append(fields, kv{key: key.Str, val: val})
			}
			return true
		})
		sort.SliceStable(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
		w.tag('{')
		for _, f := range fields {
			w.str(f.key)
			w.value(f.val)
		}
		w.tag('}')
	}
}
