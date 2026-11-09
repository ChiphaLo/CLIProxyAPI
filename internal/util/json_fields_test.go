package util

import (
	"reflect"
	"strings"
	"testing"
)

func TestIndexedRootFieldsPreservesLookupSemantics(t *testing.T) {
	for _, raw := range []string{
		`{"input":[{"text":"` + strings.Repeat("DATA", 10000) + `"}],"reasoning":{"effort":"high"},"null":null,"a.b":7}`,
		`{"a":{"x":1},"a":{"b":2},"a":null,"escaped":{"a.b":5}}`,
		`[1,2,{"a":3}]`,
	} {
		root := ParseGJSONBytesNoCopy([]byte(raw))
		get := IndexRootFields(root)
		for _, path := range []string{"input", "missing", "missing.a", "reasoning.effort", "a.b", "a.x", "null", `a\.b`, `escaped.a\.b`, "i*", "input.#", "input.0.text", "0", "2.a", "@this"} {
			if got, want := get(path), root.Get(path); !reflect.DeepEqual(got, want) {
				t.Fatalf("lookup %s differs: got %#v, want %#v", path, got, want)
			}
		}
	}
}
