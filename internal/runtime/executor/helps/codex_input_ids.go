package helps

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexInputItemIDLimit                 = 64
	codexMessageItemIDPrefix              = "msg"
	codexReasoningItemIDPrefix            = "rs"
	codexFunctionCallItemIDPrefix         = "fc"
	codexCustomToolCallItemIDPrefix       = "ctc"
	codexCustomToolCallOutputItemIDPrefix = "ctco"
	codexWebSearchItemIDPrefix            = "ws"

	codexInputItemIDOccupied  uint8 = 1 << 0
	codexInputItemIDPreserved uint8 = 1 << 1
)

// SanitizeCodexInputItemIDs normalizes input item ID characters and prefixes for Codex, removes encrypted
// reasoning items whose IDs exceed the Codex limit, and deterministically shortens
// other overlong input item IDs. Client tool call IDs and their results share
// one portable mapping. Foreign search result records are preserved as
// historical text beside their native search action and source URLs.
func SanitizeCodexInputItemIDs(body []byte) []byte {
	input := util.GetGJSONBytesNoCopy(body, "input")
	if !input.IsArray() {
		return body
	}

	items := input.Array()
	hasReferences := false
	callIDUsed := make(map[string]bool)
	idStates := make(map[string]uint8, len(items))
	for _, item := range items {
		if item.Get("type").String() == "item_reference" {
			hasReferences = true
		}
		if shouldDropCodexEncryptedReasoningItem(item) {
			continue
		}
		if callID := item.Get("call_id"); callID.Type == gjson.String && isPortableToolCallID(callID.String()) && len(callID.String()) <= codexInputItemIDLimit {
			callIDUsed[callID.String()] = true
		}
		itemID := item.Get("id")
		if itemID.Type != gjson.String {
			continue
		}
		originalID := itemID.String()
		id := normalizeCodexInputItemID(item, originalID)
		state := idStates[id]
		if id == originalID {
			state |= codexInputItemIDPreserved
		}
		if len([]rune(id)) <= codexInputItemIDLimit {
			state |= codexInputItemIDOccupied
		}
		if state != 0 {
			idStates[id] = state
		}
	}

	var mapped map[string]string
	var collisionMapped map[string]string
	var aliases map[string]string
	var callIDs map[string]string
	rebuilt := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		if shouldDropCodexEncryptedReasoningItem(item) {
			changed = true
			continue
		}

		raw := item.Raw
		if field := item.Get("call_id"); field.Type == gjson.String && field.String() != "" {
			original := field.String()
			if !isPortableToolCallID(original) || len(original) > codexInputItemIDLimit {
				id := callIDs[original]
				if id == "" {
					normalized := original
					if !isPortableToolCallID(original) {
						normalized = "call_" + base64.RawURLEncoding.EncodeToString([]byte(original))
					}
					id = shortenCodexInputItemID(normalized)
					for attempt := 0; callIDUsed[id]; attempt++ {
						id = codexInputItemIDWithHashSuffix(normalized, attempt)
					}
					if callIDs == nil {
						callIDs = make(map[string]string)
					}
					callIDs[original] = id
					callIDUsed[id] = true
				}
				if next, errSet := sjson.Set(raw, "call_id", id); errSet == nil {
					raw = next
					changed = true
				}
			}
		}
		itemID := item.Get("id")
		if itemID.Type == gjson.String {
			originalID := itemID.String()
			id := normalizeCodexInputItemID(item, originalID)
			if id != originalID && idStates[id]&codexInputItemIDPreserved != 0 {
				collisionID, ok := collisionMapped[id]
				if !ok {
					for attempt := 0; ; attempt++ {
						collisionID = codexInputItemIDWithHashSuffix(id, attempt)
						if idStates[collisionID]&codexInputItemIDOccupied != 0 {
							continue
						}
						if collisionMapped == nil {
							collisionMapped = make(map[string]string)
						}
						collisionMapped[id] = collisionID
						idStates[collisionID] |= codexInputItemIDOccupied
						break
					}
				}
				id = collisionID
			}
			if len([]rune(id)) > codexInputItemIDLimit {
				shortened, ok := mapped[id]
				if !ok {
					shortened = shortenCodexInputItemID(id)
					for attempt := 1; ; attempt++ {
						if idStates[shortened]&codexInputItemIDOccupied == 0 {
							break
						}
						shortened = shortenCodexInputItemIDWithAttempt(id, attempt)
					}
					if mapped == nil {
						mapped = make(map[string]string)
					}
					mapped[id] = shortened
					idStates[shortened] |= codexInputItemIDOccupied
				}
				id = shortened
			}

			if id != originalID {
				next, errSet := sjson.SetBytes([]byte(raw), "id", id)
				if errSet == nil {
					raw = string(next)
					changed = true
					if hasReferences && item.Get("type").String() != "item_reference" {
						if aliases == nil {
							aliases = make(map[string]string)
						}
						aliases[originalID] = id
					}
				}
			}
		}
		rebuilt = append(rebuilt, raw)
		// Foreign search adapters expose untyped result records. Keep the native
		// search action and preserve their complete payload as historical text;
		// OpenAI validates results against its own typed result schema.
		results := item.Get("results")
		if item.Get("type").String() == "web_search_call" && results.IsArray() {
			foreign := false
			for _, result := range results.Array() {
				if !result.Get("type").Exists() {
					foreign = true
					break
				}
			}
			if foreign {
				raw, _ = sjson.Delete(raw, "results")
				if item.Get("action.type").String() == "search" && !item.Get("action.sources").Exists() {
					sources := []map[string]string{}
					for _, result := range results.Array() {
						if url := result.Get("url").String(); url != "" {
							sources = append(sources, map[string]string{"type": "url", "url": url})
						}
					}
					if len(sources) > 0 {
						raw, _ = sjson.Set(raw, "action.sources", sources)
					}
				}
				rebuilt[len(rebuilt)-1] = raw
				message, _ := json.Marshal(map[string]any{
					"type": "message", "role": "assistant", "phase": "commentary",
					"content": []map[string]string{{"type": "output_text", "text": "Historical web search results:\n" + results.Raw}},
				})
				rebuilt = append(rebuilt, string(message))
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	if len(aliases) > 0 {
		for i, raw := range rebuilt {
			item := gjson.Parse(raw)
			if item.Get("type").String() != "item_reference" {
				continue
			}
			if id := aliases[item.Get("id").String()]; id != "" {
				if next, errSet := sjson.Set(raw, "id", id); errSet == nil {
					rebuilt[i] = next
				}
			}
		}
	}

	updated, errSet := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if errSet != nil {
		return body
	}
	return updated
}

func normalizeCodexInputItemID(item gjson.Result, id string) string {
	var prefix string
	switch item.Get("type").String() {
	case "message":
		prefix = codexMessageItemIDPrefix
	case "reasoning":
		prefix = codexReasoningItemIDPrefix
	case "function_call":
		prefix = codexFunctionCallItemIDPrefix
	case "custom_tool_call":
		prefix = codexCustomToolCallItemIDPrefix
	case "custom_tool_call_output":
		prefix = codexCustomToolCallOutputItemIDPrefix
	case "web_search_call":
		if id == "" || isPortableToolCallID(id) {
			return id
		}
		prefix = codexWebSearchItemIDPrefix
	case "item_reference":
		return id
	}
	if id != "" && !isPortableToolCallID(id) {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(id))
		if prefix == "" {
			return "item_" + encoded
		}
		return prefix + "_" + encoded
	}
	if prefix == "" || id == "" || strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + "_" + id
}

func shouldDropCodexEncryptedReasoningItem(item gjson.Result) bool {
	if item.Get("type").String() != "reasoning" {
		return false
	}
	itemID := item.Get("id")
	if itemID.Type != gjson.String || len([]rune(itemID.String())) <= codexInputItemIDLimit {
		return false
	}
	encryptedContent := item.Get("encrypted_content")
	return encryptedContent.Type == gjson.String && encryptedContent.String() != ""
}

func shortenCodexInputItemID(id string) string {
	return shortenCodexInputItemIDWithAttempt(id, 0)
}

func shortenCodexInputItemIDWithAttempt(id string, attempt int) string {
	runes := []rune(id)
	if len(runes) <= codexInputItemIDLimit {
		return id
	}
	return codexInputItemIDWithHashSuffixRunes(id, runes, attempt)
}

func codexInputItemIDWithHashSuffix(id string, attempt int) string {
	return codexInputItemIDWithHashSuffixRunes(id, []rune(id), attempt)
}

func codexInputItemIDWithHashSuffixRunes(id string, runes []rune, attempt int) string {
	hashInput := id
	if attempt > 0 {
		hashInput += "\x00" + strconv.Itoa(attempt)
	}
	sum := sha256.Sum256([]byte(hashInput))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLength := codexInputItemIDLimit - len(suffix)
	if len(runes) < prefixLength {
		prefixLength = len(runes)
	}
	return string(runes[:prefixLength]) + suffix
}
