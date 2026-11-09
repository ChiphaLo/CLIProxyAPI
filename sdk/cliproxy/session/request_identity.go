package session

import (
	"encoding/json"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const requestIdentityMetadataKey = "cpa:request-identity"

// RequestIdentity owns read-only request analysis. Only SDK-created values are
// accepted; JSON metadata from a client cannot manufacture this type.
type RequestIdentity struct {
	payload   []byte
	root      sessionObject
	firstUser string
	userJSON  string
}

func requestIdentity(payload []byte, metadata map[string]any) *RequestIdentity {
	value, _ := metadata[requestIdentityMetadataKey].(*RequestIdentity)
	if value == nil || len(payload) != len(value.payload) || len(payload) == 0 || &payload[0] != &value.payload[0] {
		return nil
	}
	return value
}

func (identity *RequestIdentity) derive(format sdktranslator.Format, scope string, cacheUser bool) string {
	var fields map[string]ojson.Result
	if len(identity.root.duplicates) == 0 {
		fields = identity.root.fields
	}
	root, first, ok := selectiveCanonicalRootWithFields(format, identity.payload, nil, "", fields)
	if !ok {
		return DeriveID(format, identity.payload, scope)
	}
	if len(root.User) == 0 {
		return ""
	}
	root.CallerScope = strings.TrimSpace(scope)
	identity.firstUser = first
	// Only internal identity forwarding needs a reusable canonical user JSON.
	// Other routes stream canonical parts directly to the identity hash.
	identity.userJSON = ""
	if cacheUser {
		if raw, err := json.Marshal(root.User); err == nil {
			// Marshal returns private immutable bytes. Borrow their JSON string
			// so repeated Doc.SetRaw calls do not copy the canonical media.
			identity.userJSON = util.ParseGJSONBytesNoCopy(raw).Raw
		}
	}
	return hashRootWithUser(root, identity.userJSON)
}

// ForwardDerivedID preserves the destination's caller namespace and the final
// payload's instructions, while reusing unchanged normalized user content.
func ForwardDerivedID(payload []byte, format sdktranslator.Format, scope string, metadata map[string]any) string {
	identity, _ := metadata[requestIdentityMetadataKey].(*RequestIdentity)
	var reuse *RequestIdentity
	var first string
	if identity != nil && identity.userJSON != "" {
		reuse, first = identity, identity.firstUser
	}
	indexed := newSessionObject(util.ParseGJSONBytesNoCopy(payload))
	if hasExplicitSessionFromRoot(nil, payload, indexed) {
		return ""
	}
	var fields map[string]ojson.Result
	if len(indexed.duplicates) == 0 {
		fields = indexed.fields
	}
	root, selected, ok := selectiveCanonicalRootWithFields(format, payload, reuse, first, fields)
	if !ok {
		return DeriveID(format, payload, scope)
	}
	reused := identity != nil && identity.userJSON != "" && selected == identity.firstUser
	if len(root.User) == 0 && !reused {
		return ""
	}
	root.CallerScope = strings.TrimSpace(scope)
	if reused {
		return hashRootWithUser(root, identity.userJSON)
	}
	return hashRoot(root)
}

func selectiveCanonicalRoot(format sdktranslator.Format, payload []byte) (canonicalRoot, string, bool) {
	return selectiveCanonicalRootWithFields(format, payload, nil, "", nil)
}

func selectiveCanonicalRootWithFields(format sdktranslator.Format, payload []byte, reuse *RequestIdentity, userRaw string, fields map[string]ojson.Result) (canonicalRoot, string, bool) {
	root := canonicalRoot{Version: identityVersion, Format: format.String()}
	if !ojson.ValidBytes(payload) {
		return root, "", true
	}
	if fields == nil {
		fields = map[string]ojson.Result{}
		util.ParseGJSONBytesNoCopy(payload).ForEach(func(k, v ojson.Result) bool { fields[k.String()] = v; return true })
	}
	var items ojson.Result
	responses := sourceFormatEqual(format, sdktranslator.FormatOpenAIResponse) || sourceFormatEqual(format, sdktranslator.FormatCodex)
	gemini := sourceFormatEqual(format, sdktranslator.FormatGemini) || sourceFormatEqual(format, sdktranslator.FormatAntigravity)
	switch {
	case responses:
		root.Instructions = appendInstruction(root.Instructions, fields["instructions"].Value())
		items = fields["input"]
		if items.Type == ojson.String {
			if reuse == nil || items.Raw != userRaw {
				root.User = canonicalParts(items.String())
			}
			return root, items.Raw, true
		}
	case gemini:
		if req := fields["request"]; req.IsObject() {
			fields = map[string]ojson.Result{}
			req.ForEach(func(k, v ojson.Result) bool { fields[k.String()] = v; return true })
		}
		for _, key := range []string{"cachedContent", "cached_content"} {
			if v, exists := fields[key]; exists {
				if v.Type == ojson.String {
					root.Resource = strings.TrimSpace(v.String())
				}
				break
			}
		}
		for _, key := range []string{"systemInstruction", "system_instruction"} {
			if v, ok := fields[key]; ok {
				root.Instructions = appendInstruction(root.Instructions, contentValue(v.Value()))
				break
			}
		}
		items = fields["contents"]
	case sourceFormatEqual(format, sdktranslator.FormatInteractions):
		return root, "", false
	default:
		if sourceFormatEqual(format, sdktranslator.FormatClaude) {
			root.Instructions = appendInstruction(root.Instructions, fields["system"].Value())
		}
		items = fields["messages"]
	}
	first := ""
	if !items.IsArray() {
		return root, first, true
	}
	items.ForEach(func(_, item ojson.Result) bool {
		var role, content, parts, text ojson.Result
		contentSeen, partsSeen, textSeen := false, false, false
		item.ForEach(func(k, v ojson.Result) bool {
			switch k.String() {
			case "role":
				role = v
			case "content":
				content = v
				contentSeen = true
			case "parts":
				parts = v
				partsSeen = true
			case "text":
				text = v
				textSeen = true
			}
			return true
		})
		if gemini && !contentSeen {
			if partsSeen {
				content = parts
			} else if textSeen {
				content = text
			} else {
				content = item
			}
		}
		if role.Type != ojson.String {
			return true
		}
		kind := strings.ToLower(strings.TrimSpace(role.String()))
		if kind == "user" {
			if reuse != nil && content.Raw == userRaw && content.Raw != "" {
				first = content.Raw
				return false
			} else {
				root.User = canonicalParts(content.Value())
			}
			if len(root.User) > 0 {
				first = content.Raw
				return false
			}
		} else if !gemini && (kind == "system" || kind == "developer") {
			root.Instructions = appendInstruction(root.Instructions, content.Value())
		}
		return true
	})
	return root, first, true
}

func (*RequestIdentity) String() string { return "request-scoped identity" }
