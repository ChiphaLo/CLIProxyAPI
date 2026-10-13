package models

import "testing"

func TestReviewedClientHintsKeepDistinctWindowsAndSupportedDefault(t *testing.T) {
	entry := map[string]any{"context_window": 128000, "max_context_window": 128000, "default_reasoning_level": "high", "supported_reasoning_levels": []any{map[string]any{"effort": "high"}, map[string]any{"effort": "ultra"}}}
	applyReviewedClientHints(entry, map[string]any{"client_context_window": 272000, "client_max_context_window": 1000000, "client_default_reasoning_level": "ultra"})
	if entry["context_window"] != 272000 || entry["max_context_window"] != 1000000 || entry["default_reasoning_level"] != "ultra" {
		t.Fatal("reviewed client hints were collapsed or lost")
	}
	applyReviewedClientHints(entry, map[string]any{"client_default_reasoning_level": "unsupported"})
	if entry["default_reasoning_level"] != "ultra" {
		t.Fatal("an unadvertised default reasoning level was inserted")
	}
}
