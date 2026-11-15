package helps

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
)

func dimensionFixture(t *testing.T, width, height int, format string) string {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 230, A: 128})
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	default:
		err = png.Encode(&b, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func dimensionDoc(t *testing.T, data, format string, count int, nested bool) *ojson.Doc {
	t.Helper()
	doc := ojson.NewDoc([]byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":[]}],"metadata":{"sentinel":"unchanged"}}`))
	for n := 0; n < count-1; n++ {
		_ = doc.Set("messages.0.content.-1", map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.invalid/image.png"}})
	}
	block := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/" + format, "data": data}}
	if nested {
		_ = doc.Set("messages.0.content.-1", map[string]any{"type": "tool_result", "tool_use_id": "call-a", "content": []any{map[string]any{"type": "text", "text": "keep text"}, block}})
	} else {
		_ = doc.Set("messages.0.content.-1", block)
	}
	return doc
}

func TestClaudeManyImageDimensionBoundary(t *testing.T) {
	data := dimensionFixture(t, 3474, 16, "png")
	for _, count := range []int{20, 21} {
		doc := dimensionDoc(t, data, "png", count, false)
		before := bytes.Clone(doc.Bytes())
		n, err := ResizeClaudeImagesDoc(doc)
		if err != nil {
			t.Fatal(err)
		}
		if count == 20 {
			if n != 0 || !bytes.Equal(before, doc.Bytes()) {
				t.Fatal("allowed image changed")
			}
			continue
		}
		if n != 1 {
			t.Fatal("many-image resize missing")
		}
		encoded := doc.Get("messages.0.content.20.source.data").String()
		cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
		if err != nil || cfg.Width != 3000 || cfg.Height != 13 {
			t.Fatalf("dimensions: %v %v", cfg, err)
		}
		if len(doc.Get("messages.0.content").Array()) != 21 || doc.Get("metadata.sentinel").String() != "unchanged" {
			t.Fatal("context structure changed")
		}
		after := bytes.Clone(doc.Bytes())
		n, err = ResizeClaudeImagesDoc(doc)
		if err != nil || n != 0 || !bytes.Equal(after, doc.Bytes()) {
			t.Fatal("repeated processing changed image")
		}
	}
}

func TestClaudeNestedToolImagesAndDocuments(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif"} {
		doc := dimensionDoc(t, dimensionFixture(t, 8, 3001, format), format, 20, true)
		_ = doc.Set("messages.0.content.-1", map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "document-sentinel"}})
		n, err := ResizeClaudeImagesDoc(doc)
		if err != nil || n != 1 {
			t.Fatalf("%s: %d %v", format, n, err)
		}
		path := "messages.0.content.19.content.1.source"
		cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(doc.Get(path+".data").String())))
		if err != nil || cfg.Height != 3000 || cfg.Width != 7 {
			t.Fatalf("%s: %v %v", format, cfg, err)
		}
		if doc.Get("messages.0.content.19.tool_use_id").String() != "call-a" || doc.Get("messages.0.content.19.content.0.text").String() != "keep text" || doc.Get("messages.0.content.20.source.data").String() != "document-sentinel" {
			t.Fatal("tool pairing or document changed")
		}
	}
}

func TestClaudeImageCacheAndSingleImageLimit(t *testing.T) {
	data := dimensionFixture(t, 8001, 2, "png")
	for _, count := range []int{1, 21, 21} {
		doc := dimensionDoc(t, data, "png", count, false)
		n, err := ResizeClaudeImagesDoc(doc)
		if err != nil || n != 1 {
			t.Fatalf("resize: %d %v", n, err)
		}
		parts := doc.Get("messages.0.content").Array()
		cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(parts[len(parts)-1].Get("source.data").String())))
		want := 8000
		if count > 20 {
			want = 3000
		}
		if err != nil || cfg.Width != want {
			t.Fatalf("wrong cached limit: %v %v", cfg, err)
		}
	}
	doc := dimensionDoc(t, data, "png", 21, false)
	_ = doc.Set("model", "kimi-k2.5")
	before := bytes.Clone(doc.Bytes())
	n, err := ResizeClaudeImagesDoc(doc)
	if err != nil || n != 0 || !bytes.Equal(before, doc.Bytes()) {
		t.Fatal("other provider payload changed")
	}
}
