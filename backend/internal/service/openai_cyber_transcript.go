package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type openAICyberTranscriptBlockKeys struct {
	lookupKeys          []string
	preLatestUserKey    string
	lookupKeysTruncated bool
	// itemCount 是参与哈希的条目数；第 n 个前缀 key 的位置即 n。
	itemCount int
	// preLatestUserPos 是 preLatestUserKey 对应的前缀位置（0 表示无）。
	preLatestUserPos int
	// positionKeys 是调用方指定位置上、已滑出 lookupKeys 窗口的前缀 key。
	positionKeys []string
}

// Bound the Redis lookup work for a single request while retaining the most
// recent transcript prefixes, where a continuation is most likely to match.
const maxOpenAICyberTranscriptLookupKeys = 256

// deriveOpenAICyberTranscriptBlockKeys returns cumulative semantic-history
// hashes plus the context key immediately before the latest user turn. The
// context key requires model-generated history so shared first-turn templates
// cannot block unrelated conversations.
//
// positions 是命中时记录下来的被屏蔽前缀位置：对超出 lookupKeys 窗口的长对话，
// 额外返回这些位置上的前缀 key，使续写会话无论追加多少条目都能精确匹配。
func deriveOpenAICyberTranscriptBlockKeys(apiKeyID int64, body []byte, positions ...int) openAICyberTranscriptBlockKeys {
	if len(body) == 0 {
		return openAICyberTranscriptBlockKeys{}
	}
	root := openAIRequestPayloadView(body)
	if !root.Exists() || !root.IsObject() {
		return openAICyberTranscriptBlockKeys{}
	}

	h := sha256.New()
	_, _ = h.Write([]byte("cyber-transcript:v3|api_key="))
	_, _ = h.Write([]byte(strconv.FormatInt(apiKeyID, 10)))
	// Model and tool definitions are request configuration rather than history.
	// Root instructions remain model-visible context and participate in identity.
	for _, field := range []string{"instructions"} {
		v := root.Get(field)
		if !v.Exists() || (v.Type == gjson.String && strings.TrimSpace(v.String()) == "") {
			continue
		}
		canonical := normalizeCompatSeedJSON(json.RawMessage(v.Raw))
		if v.Type == gjson.String {
			canonical = v.String()
		}
		_, _ = h.Write([]byte("|"))
		_, _ = h.Write([]byte(field))
		_, _ = h.Write([]byte("="))
		_, _ = h.Write([]byte(canonical))
	}

	appendSequence := func(sequence gjson.Result) openAICyberTranscriptBlockKeys {
		if !sequence.Exists() || !sequence.IsArray() {
			return openAICyberTranscriptBlockKeys{}
		}
		result := openAICyberTranscriptBlockKeys{
			lookupKeys: make([]string, 0, maxOpenAICyberTranscriptLookupKeys),
		}
		nextLookupKey := 0
		lookupKeysRotated := false
		lastLookupKey := ""
		wanted := make(map[int]struct{}, len(positions))
		for _, pos := range positions {
			if pos > 0 {
				wanted[pos] = struct{}{}
			}
		}
		positionKeysByPos := make(map[int]string, len(wanted))
		// This is an entropy heuristic, not provenance proof: authenticated
		// server-side history would be required to distinguish fixed few-shot
		// assistant items perfectly.
		hasModelGeneratedItem := false
		sequence.ForEach(func(_, item gjson.Result) bool {
			canonical := item.Raw
			switch item.Type {
			case gjson.String:
				encoded, _ := json.Marshal(item.String())
				canonical = string(encoded)
			case gjson.JSON:
				canonical = normalizeCompatSeedJSON(json.RawMessage(item.Raw))
			}
			if strings.TrimSpace(canonical) == "" {
				return true
			}
			if openAICyberTranscriptItemStartsUserTurn(item) && hasModelGeneratedItem && lastLookupKey != "" {
				result.preLatestUserKey = lastLookupKey
				result.preLatestUserPos = result.itemCount
			}
			_, _ = h.Write([]byte("|item="))
			_, _ = h.Write([]byte(canonical))
			lastLookupKey = hex.EncodeToString(h.Sum(nil))
			result.itemCount++
			if _, ok := wanted[result.itemCount]; ok {
				positionKeysByPos[result.itemCount] = lastLookupKey
			}
			if len(result.lookupKeys) < maxOpenAICyberTranscriptLookupKeys {
				result.lookupKeys = append(result.lookupKeys, lastLookupKey)
			} else {
				result.lookupKeys[nextLookupKey] = lastLookupKey
				nextLookupKey = (nextLookupKey + 1) % maxOpenAICyberTranscriptLookupKeys
				lookupKeysRotated = true
				result.lookupKeysTruncated = true
			}
			if openAICyberTranscriptItemIsModelGenerated(item) {
				hasModelGeneratedItem = true
			}
			return true
		})
		if lookupKeysRotated {
			ordered := make([]string, 0, len(result.lookupKeys))
			ordered = append(ordered, result.lookupKeys[nextLookupKey:]...)
			ordered = append(ordered, result.lookupKeys[:nextLookupKey]...)
			result.lookupKeys = ordered
		}
		// 窗口内的位置已由 lookupKeys 覆盖，只补窗口外的。
		windowStart := result.itemCount - len(result.lookupKeys)
		for _, pos := range positions {
			if key, ok := positionKeysByPos[pos]; ok && pos <= windowStart {
				result.positionKeys = append(result.positionKeys, key)
				delete(positionKeysByPos, pos)
			}
		}
		return result
	}

	if messages := root.Get("messages"); messages.Exists() {
		return appendSequence(messages)
	}
	input := root.Get("input")
	if input.IsArray() {
		return appendSequence(input)
	}
	if input.Type == gjson.String && strings.TrimSpace(input.String()) != "" {
		encoded, _ := json.Marshal(input.String())
		_, _ = h.Write([]byte("|item="))
		_, _ = h.Write(encoded)
		return openAICyberTranscriptBlockKeys{lookupKeys: []string{hex.EncodeToString(h.Sum(nil))}, itemCount: 1}
	}
	return openAICyberTranscriptBlockKeys{}
}

func openAICyberTranscriptItemStartsUserTurn(item gjson.Result) bool {
	if strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
		content := item.Get("content")
		if content.IsArray() {
			hasUserContent := false
			content.ForEach(func(_, block gjson.Result) bool {
				switch strings.ToLower(strings.TrimSpace(block.Get("type").String())) {
				case "tool_result", "function_call_output", "custom_tool_call_output", "computer_call_output":
				default:
					hasUserContent = true
				}
				return !hasUserContent
			})
			return hasUserContent
		}
		return content.Exists()
	}
	return strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "input_text")
}

func openAICyberTranscriptItemIsModelGenerated(item gjson.Result) bool {
	switch strings.ToLower(strings.TrimSpace(item.Get("role").String())) {
	case "assistant", "model":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "output_text", "function_call", "tool_call", "custom_tool_call", "computer_call":
		return true
	default:
		return false
	}
}
