// Package opencode contains request-scoped compatibility gates for OpenCode.
// It never executes tools or authorizes tools based on provider-generated text.
package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	MaxChars    = 1024 * 1024
	MaxCalls    = 32
	MaxParams   = 128
	FormatError = "上游返回了不完整或不符合本次工具声明的 DSML 调用。本次未执行该调用，请重试或切换模型。"
)

var markers = []string{"<｜｜DSML｜｜ calls>", "<|DSML| calls>"}

// PrefixKind also recognizes incomplete and broken start-of-turn roots.
func PrefixKind(text string) string {
	start := strings.TrimLeftFunc(text, unicode.IsSpace)
	if start == "" {
		return "pending"
	}
	for _, marker := range markers {
		if strings.HasPrefix(marker, start) {
			return "pending"
		}
		if strings.HasPrefix(start, marker) {
			return "dsml"
		}
	}
	if strings.HasPrefix(start, "<｜｜DSML｜｜") || strings.HasPrefix(start, "<|DSML|") {
		return "dsml"
	}
	return "text"
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type offlineLoader struct{}

func (offlineLoader) Load(_ string) (any, error) {
	return nil, fmt.Errorf("external schema resources are disabled")
}

func checkRefs(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" || key == "$dynamicRef" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("external schema reference")
				}
			}
			if err := checkRefs(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := checkRefs(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// strictJSON rejects duplicate object keys at every depth and trailing values.
// Arguments remain data; there is no eval, shell, or language conversion.
func strictJSON(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	value, err := jsonValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing parameter JSON")
	}
	return value, nil
}

func jsonValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, fmt.Errorf("parameter JSON depth limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid parameter JSON")
	}
	delim, composite := token.(json.Delim)
	if !composite {
		if number, ok := token.(json.Number); ok {
			text := number.String()
			if strings.ContainsAny(text, ".eE") {
				// Python's JSON parser uses binary64 for these values. Normalize
				// before schema validation to prevent huge-exponent big.Rat work.
				value, err := strconv.ParseFloat(text, 64)
				if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
					return nil, fmt.Errorf("non-finite parameter JSON number")
				}
				return value, nil
			}
			if len(strings.TrimPrefix(text, "-")) > 4300 {
				// Match the legacy Python default integer-conversion digit cap.
				return nil, fmt.Errorf("parameter integer digit limit")
			}
		}
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("invalid parameter JSON")
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("invalid property name")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON property")
			}
			value, err := jsonValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("incomplete parameter JSON")
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := jsonValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("incomplete parameter JSON")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected parameter JSON delimiter")
	}
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	value, err := strictJSON(string(raw))
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool schema must be an object")
	}
	if err := checkRefs(object); err != nil {
		return nil, err
	}
	// The legacy implementation explicitly uses Draft202012Validator, regardless
	// of a schema's dialect annotation. Do the same without fetching metaschemas.
	object["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(offlineLoader{})
	const location = "urn:cpa:request-tool-schema"
	if err := compiler.AddResource(location, object); err != nil {
		return nil, fmt.Errorf("invalid tool schema")
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("invalid tool schema")
	}
	return schema, nil
}

// RecoverCalls validates the entire envelope before returning any call.
// Names, schemas, and allowed tool choices must come from this request.
func RecoverCalls(text string, declarations map[string]json.RawMessage, allowed map[string]bool) ([]ToolCall, error) {
	if utf8.RuneCountInString(text) > MaxChars {
		return nil, fmt.Errorf("envelope size limit")
	}
	source := strings.TrimSpace(text)
	marker := ""
	for _, candidate := range markers {
		if strings.HasPrefix(source, candidate) {
			marker = candidate
			break
		}
	}
	if marker == "" {
		return nil, fmt.Errorf("invalid calls root")
	}
	token := marker[1:strings.Index(marker, " calls>")]
	closeRoot := "</" + token + " calls>"
	if !strings.HasSuffix(source, closeRoot) {
		return nil, fmt.Errorf("incomplete calls root")
	}
	inner := source[len(marker) : len(source)-len(closeRoot)]
	tag := regexp.QuoteMeta(token)
	invoke := regexp.MustCompile(`(?s)^<` + tag + ` invoke name="([A-Za-z0-9_.:-]+)">(.*?)</` + tag + ` invoke>`)
	parameter := regexp.MustCompile(`(?s)^<` + tag + ` parameter name="([A-Za-z0-9_.-]+)" string="(true|false)">(.*?)</` + tag + ` parameter>`)
	calls := []ToolCall{}
	compiled := map[string]*jsonschema.Schema{}
	for strings.TrimSpace(inner) != "" {
		inner = strings.TrimSpace(inner)
		match := invoke.FindStringSubmatchIndex(inner)
		if match == nil {
			return nil, fmt.Errorf("unexpected text between invokes")
		}
		name, content := inner[match[2]:match[3]], inner[match[4]:match[5]]
		inner = inner[match[1]:]
		declaration, declared := declarations[name]
		if !allowed[name] || !declared {
			return nil, fmt.Errorf("undeclared or disallowed tool")
		}
		args := map[string]any{}
		for strings.TrimSpace(content) != "" {
			content = strings.TrimSpace(content)
			param := parameter.FindStringSubmatchIndex(content)
			if param == nil {
				return nil, fmt.Errorf("invalid parameter envelope")
			}
			key, stringFlag, value := content[param[2]:param[3]], content[param[4]:param[5]], content[param[6]:param[7]]
			content = content[param[1]:]
			if _, duplicate := args[key]; duplicate || len(args) >= MaxParams {
				return nil, fmt.Errorf("duplicate/excessive parameters")
			}
			if strings.Contains(value, "<"+token) || strings.Contains(value, "</"+token) {
				return nil, fmt.Errorf("nested DSML markers")
			}
			if stringFlag == "true" {
				args[key] = value
			} else {
				parsed, err := strictJSON(value)
				if err != nil {
					return nil, err
				}
				args[key] = parsed
			}
		}
		schema := compiled[name]
		if schema == nil {
			var err error
			schema, err = compileSchema(declaration)
			if err != nil {
				return nil, err
			}
			compiled[name] = schema
		}
		if err := schema.Validate(args); err != nil {
			return nil, fmt.Errorf("tool arguments do not match declaration")
		}
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(args); err != nil {
			return nil, fmt.Errorf("cannot serialize tool arguments")
		}
		call := ToolCall{ID: "call_dsml_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24], Type: "function"}
		call.Function.Name, call.Function.Arguments = name, strings.TrimSuffix(buf.String(), "\n")
		calls = append(calls, call)
		if len(calls) > MaxCalls {
			return nil, fmt.Errorf("too many calls")
		}
	}
	if len(calls) == 0 {
		return nil, fmt.Errorf("empty calls")
	}
	return calls, nil
}
