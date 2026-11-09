package util

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// IndexRootFields avoids rescanning large histories for independent root
// lookups. Values reference root; callers must keep its input immutable.
func IndexRootFields(root ojson.Result) func(string) ojson.Result {
	fields := make(map[string]ojson.Result)
	duplicates := make(map[string]bool)
	root.ForEach(func(key, value ojson.Result) bool {
		name := key.String()
		if _, exists := fields[name]; exists {
			duplicates[name] = true
		} else {
			fields[name] = value
		}
		return true
	})
	return func(path string) ojson.Result {
		key, rest, nested := strings.Cut(path, ".")
		if !root.IsObject() || duplicates[key] || strings.ContainsAny(path, `\*?#!@|[]`) {
			return root.Get(path)
		}
		value := fields[key]
		if nested && value.Exists() {
			return value.Get(rest)
		}
		if nested {
			return ojson.Result{}
		}
		return value
	}
}
