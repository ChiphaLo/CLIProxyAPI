package configbundle

import (
	"strings"
	"testing"
)

func TestManagementJSONStructuralBounds(t *testing.T) {
	var value any
	if DecodeDraft([]byte(strings.Repeat("[", 80)+"0"+strings.Repeat("]", 80)), &value) == nil {
		t.Fatal("deeply nested management/catalog JSON accepted")
	}
	if DecodeDraft([]byte("["+strings.Repeat("0,", 200001)+"0]"), &value) == nil {
		t.Fatal("excessive management/catalog nodes accepted")
	}
	if err := DecodeDraft([]byte(`{"models":[{"id":"fixture","capabilities":{"input":["text","image"]}}]}`), &value); err != nil {
		t.Fatal("normal capability structure was rejected")
	}
}
