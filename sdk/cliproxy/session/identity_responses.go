package session

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Only leading instructions and the first complete user input define the root.
func deriveResponsesID(format sdktranslator.Format, payload []byte, callerScope string) string {
	if !ojson.ValidBytes(payload) {
		return ""
	}
	root := canonicalRoot{Version: identityVersion, Format: format.String(), CallerScope: strings.TrimSpace(callerScope)}
	var input, instructions ojson.Result
	util.ParseGJSONBytesNoCopy(payload).ForEach(func(key, value ojson.Result) bool {
		switch key.String() {
		case "input":
			input = value
		case "instructions":
			instructions = value
		}
		return true
	})
	root.Instructions = appendInstruction(root.Instructions, instructions.Value())
	if input.Type == ojson.String {
		root.User = canonicalParts(input.String())
	} else if input.IsArray() {
		input.ForEach(func(_, item ojson.Result) bool {
			var role, content ojson.Result
			item.ForEach(func(key, value ojson.Result) bool {
				switch key.String() {
				case "role":
					role = value
				case "content":
					content = value
				}
				return true
			})
			if role.Type != ojson.String {
				return true
			}
			kind := strings.ToLower(strings.TrimSpace(role.String()))
			if kind != "developer" && kind != "system" && kind != "user" {
				return true
			}
			value := content.Value()
			if kind == "user" {
				root.User = canonicalParts(value)
				return len(root.User) == 0
			}
			root.Instructions = appendInstruction(root.Instructions, value)
			return true
		})
	}
	if len(root.User) == 0 {
		return ""
	}
	return hashRoot(root)
}
