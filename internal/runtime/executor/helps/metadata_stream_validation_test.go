package helps

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestMetadataValidationLargeStringAndFragmentBoundaries(t *testing.T) {
	large := strings.Repeat("ABCD", 1<<15)
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"messages":[{"content":"` + large + `"}],"metadata":{"user_id":"ok"}}`, true},
		{`{"messages":[{"content":"` + large + `\"escaped\\\u001f"}],"metadata":{}}`, true},
		{`{"messages":[{"content":"` + large + "\x00" + `"}],"metadata":{}}`, false},
		{`{"messages":[{"content":"` + large + `"}],"metadata":{},"\u006detadata":{}}`, false},
	} {
		for _, size := range []int{1, 7, 4096, len(test.body)} {
			v := claudeRootMetadataValidator{}
			v.stack = v.stackBuf[:0]
			for at := 0; at < len(test.body); at += size {
				end := at + size
				if end > len(test.body) {
					end = len(test.body)
				}
				if _, err := v.Write([]byte(test.body[at:end])); err != nil {
					break
				}
			}
			ok := v.err == nil && v.tok == claudeTokNone && v.phase == claudePhaseDone && len(v.stack) == 0
			if ok != test.valid {
				t.Fatalf("fragment size %d: valid=%v want=%v", size, ok, test.valid)
			}
		}
		if (validateClaudeDocMetadataRoot(ojson.NewDocString(test.body)) == nil) != test.valid {
			t.Fatal("document validation changed")
		}
	}
}
