// Package billing preserves the deployed CPA accounting contract.
// It intentionally does not use SDK token normalization or upstream pricing.
package billing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

const Version = "cpa-legacy-go/1"

// Rates are configured USD prices per million tokens, not provider prices.
type Rates struct {
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Write  float64 `json:"write"`
	Output float64 `json:"output"`
}

// Row is the existing usage.jsonl schema. Output already includes any hidden
// reasoning included by the original response; reasoning must not be added.
type Row struct {
	TS                    int64   `json:"ts"`
	Model                 string  `json:"model"`
	KeyID                 string  `json:"key_id"`
	Input                 int64   `json:"input"`
	Output                int64   `json:"output"`
	Cached                int64   `json:"cached"`
	Write                 int64   `json:"write"`
	ClientIP              string  `json:"client_ip,omitempty"`
	RequestID             string  `json:"request_id,omitempty"`
	Path                  string  `json:"path,omitempty"`
	RoutedModel           string  `json:"routed_model,omitempty"`
	OAuthAccountID        string  `json:"oauth_account_id,omitempty"`
	OAuthPrefix           string  `json:"oauth_prefix,omitempty"`
	UpstreamReportedModel string  `json:"upstream_reported_model,omitempty"`
	ModelMismatch         bool    `json:"model_mismatch,omitempty"`
	DurationMS            int64   `json:"duration_ms"`
	TTFTMS                int64   `json:"ttft_ms"`
	OutputTPS             float64 `json:"output_tps"`
}

// RoundDecimal matches Python round(float, places), including binary halfway
// values. Multiplying and using math.Round would change existing receipts.
func RoundDecimal(value float64, places int) float64 {
	text := strconv.FormatFloat(value, 'f', places, 64)
	result, _ := strconv.ParseFloat(text, 64)
	return result
}

// Cost is a language-only port of calculate_usage_cost in the deployed GUI.
// In particular, existing cache-write treatment is preserved, not corrected.
func Cost(row Row, prices map[string]*Rates) float64 {
	rate := prices[row.Model]
	if rate == nil && strings.Contains(row.Model, "/") {
		rate = prices[row.Model[strings.LastIndexByte(row.Model, '/')+1:]]
	}
	if rate == nil {
		return 0
	}
	input, cached := max(row.Input, 0), max(row.Cached, 0)
	uncached := max(input-cached, 0)
	output, write := max(row.Output, 0), max(row.Write, 0)
	amount := float64(float64(uncached) * rate.Input)
	amount = float64(amount + float64(float64(cached)*rate.Cached))
	amount = float64(amount + float64(float64(write)*rate.Write))
	amount = float64(amount + float64(float64(output)*rate.Output))
	return RoundDecimal(amount/1_000_000.0, 8)
}

func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON data")
	}
	return value, nil
}

// legacyInt follows int(value or 0) for the existing numeric JSON fields.
func legacyInt(value any) (int64, error) {
	if value == nil || value == "" || value == false {
		return 0, nil
	}
	if value == true {
		return 1, nil
	}
	switch value := value.(type) {
	case json.Number:
		if result, err := value.Int64(); err == nil {
			return result, nil
		}
		parsed, err := value.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed >= math.Exp2(63) || parsed < -math.Exp2(63) {
			return 0, fmt.Errorf("invalid integer usage value")
		}
		return int64(parsed), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	default:
		return 0, fmt.Errorf("invalid integer usage type %T", value)
	}
}

// DecodeRow reads old JSONL records without changing token values or timestamps.
func DecodeRow(raw []byte) (Row, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Row{}, err
	}
	var row Row
	// Typed metadata is decoded separately so numeric-string legacy counts work.
	for key, target := range map[string]*int64{"ts": &row.TS, "input": &row.Input,
		"cached": &row.Cached, "write": &row.Write, "output": &row.Output,
		"duration_ms": &row.DurationMS, "ttft_ms": &row.TTFTMS} {
		value, errNumber := legacyInt(object[key])
		if errNumber != nil {
			return Row{}, fmt.Errorf("%s: %w", key, errNumber)
		}
		*target = value
	}
	for key, target := range map[string]*string{"model": &row.Model, "key_id": &row.KeyID,
		"client_ip": &row.ClientIP, "request_id": &row.RequestID, "path": &row.Path,
		"routed_model": &row.RoutedModel, "oauth_account_id": &row.OAuthAccountID,
		"oauth_prefix": &row.OAuthPrefix, "upstream_reported_model": &row.UpstreamReportedModel} {
		if value, ok := object[key].(string); ok {
			*target = value
		}
	}
	row.ModelMismatch, _ = object["model_mismatch"].(bool)
	if value, ok := object["output_tps"].(json.Number); ok {
		row.OutputTPS, _ = value.Float64()
	}
	return row, nil
}

// FromRawUsage is the original log_usage token extraction. Input is the raw
// response usage object, never SDK usage.Detail or canonical TokenBreakdown.
func FromRawUsage(raw []byte, metadata Row) (Row, error) {
	value, err := decodeObject(raw)
	if err != nil || value == nil {
		return Row{}, fmt.Errorf("invalid response usage object")
	}
	for key, target := range map[string]*int64{"input_tokens": &metadata.Input, "output_tokens": &metadata.Output} {
		count, errCount := legacyInt(value[key])
		if errCount != nil {
			return Row{}, errCount
		}
		*target = count
	}
	metadata.Cached, metadata.Write = 0, 0
	if details, ok := value["input_tokens_details"].(map[string]any); ok {
		metadata.Cached, err = legacyInt(details["cached_tokens"])
		if err != nil {
			return Row{}, err
		}
		metadata.Write, err = legacyInt(details["cache_write_tokens"])
		if err != nil {
			return Row{}, err
		}
	}
	if metadata.KeyID == "" {
		metadata.KeyID = "admin"
	}
	metadata.DurationMS, metadata.TTFTMS = max(metadata.DurationMS, 0), max(metadata.TTFTMS, 0)
	if metadata.DurationMS > 7*86400000 {
		metadata.DurationMS = 0
	}
	metadata.OutputTPS = 0
	if metadata.DurationMS > 0 && metadata.Output > 0 {
		metadata.OutputTPS = RoundDecimal(float64(metadata.Output)/(float64(metadata.DurationMS)/1000.0), 2)
	}
	return metadata, nil
}

// ChatUsage is the original gateway usage() mapping, including its precedence.
// Values and reasoning details are forwarded unchanged; no billing is inferred.
func ChatUsage(raw []byte) (json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage)
	for old, next := range map[string]string{"prompt_tokens": "input_tokens", "completion_tokens": "output_tokens", "total_tokens": "total_tokens"} {
		if count, ok := value[old]; ok {
			out[next] = count
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	var details map[string]json.RawMessage
	_ = json.Unmarshal(value["prompt_tokens_details"], &details)
	cached := details["cached_tokens"]
	if len(cached) == 0 || bytes.Equal(cached, []byte("null")) {
		cached = value["prompt_cache_hit_tokens"]
	}
	if len(cached) > 0 && !bytes.Equal(cached, []byte("null")) {
		mapped := map[string]json.RawMessage{"cached_tokens": cached}
		if write := details["cache_write_tokens"]; len(write) > 0 && !bytes.Equal(write, []byte("null")) {
			mapped["cache_write_tokens"] = write
		}
		out["input_tokens_details"], _ = json.Marshal(mapped)
	}
	var completion map[string]json.RawMessage
	_ = json.Unmarshal(value["completion_tokens_details"], &completion)
	if reasoning := completion["reasoning_tokens"]; len(reasoning) > 0 && !bytes.Equal(reasoning, []byte("null")) {
		out["output_tokens_details"], _ = json.Marshal(map[string]json.RawMessage{"reasoning_tokens": reasoning})
	}
	return json.Marshal(out)
}

// LoadPrices is a port of model_prices: invalid entries are skipped, not fixed.
func LoadPrices(raw []byte) (map[string]*Rates, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*Rates)
	prices, _ := root["prices"].(map[string]any)
	for name, entry := range prices {
		name = strings.TrimSpace(name)
		object, ok := entry.(map[string]any)
		if !ok || name == "" {
			continue
		}
		rate, valid := new(Rates), true
		for key, target := range map[string]*float64{"input": &rate.Input, "cached": &rate.Cached, "write": &rate.Write, "output": &rate.Output} {
			value := object[key]
			if value == nil || value == "" {
				continue
			}
			var number float64
			var errNumber error
			switch value := value.(type) {
			case json.Number:
				number, errNumber = value.Float64()
			case string:
				number, errNumber = strconv.ParseFloat(strings.TrimSpace(value), 64)
			default:
				valid = false
			}
			if errNumber != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 1_000_000 {
				valid = false
			}
			*target = RoundDecimal(number, 8)
		}
		if valid {
			result[name] = rate
		}
	}
	return result, nil
}
