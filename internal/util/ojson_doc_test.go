package util

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestForEachDocArrayElementMatchesArraySnapshot(t *testing.T) {
	const input = `{"model":"m","messages":[{"role":"user","content":"a"}, {"role":"assistant","content":[{"type":"text","text":"b"}]},{"role":"user","content":"c"}],"tail":1}`
	for name, edit := range map[string]func(*ojson.Doc){
		"unedited":      func(*ojson.Doc) {},
		"edited before": func(d *ojson.Doc) { _ = d.Set("model", "n") },
		"edited inside": func(d *ojson.Doc) {
			_ = d.Set("messages.1.content.0.text", "B")
			_ = d.Set("tail", 2)
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := ojson.NewDocString(input)
			edit(doc)
			want := ojson.Parse(doc.String()).Get("messages").Array()
			before := doc.String()
			var got []string
			ForEachDocArrayElement(doc, "messages", func(index int, element ojson.Result) bool {
				if index != len(got) {
					t.Fatalf("index = %d, want %d", index, len(got))
				}
				got = append(got, element.Raw)
				return true
			})
			if len(got) != len(want) {
				t.Fatalf("visited %d elements, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i].Raw {
					t.Fatalf("element %d = %s, want %s", i, got[i], want[i].Raw)
				}
			}
			if doc.String() != before {
				t.Fatalf("document changed: %s", doc.String())
			}
		})
	}
}

func TestForEachDocArrayElementMissingOrEmpty(t *testing.T) {
	for _, input := range []string{`{"messages":[]}`, `{"messages":"x"}`, `{}`} {
		visited := false
		ForEachDocArrayElement(ojson.NewDocString(input), "messages", func(int, ojson.Result) bool {
			visited = true
			return true
		})
		if visited {
			t.Fatalf("%s: visited an element", input)
		}
	}
}
