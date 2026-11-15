package helps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"sync"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

type claudeImagePath struct {
	path   string
	source ojson.Result
}
type claudeResizedImage struct{ data, mediaType string }
type claudeImageCacheKey struct {
	digest [32]byte
	edge   int
}

// Historical screenshots recur in every tool round. Bound cached encodings by
// both entry count and bytes; source Base64 and decoded pixels are not retained.
var claudeImageResizeCache = struct {
	sync.Mutex
	items map[claudeImageCacheKey]claudeResizedImage
	order []claudeImageCacheKey
	bytes int
}{items: make(map[claudeImageCacheKey]claudeResizedImage)}

// ResizeClaudeImagesDoc applies Anthropic's per-request image dimension limits.
// It runs after protocol conversion and before caller payload rules.
func ResizeClaudeImagesDoc(doc *ojson.Doc) (int, error) {
	model := strings.ToLower(doc.Get("model").String())
	model = model[strings.LastIndex(model, "/")+1:]
	if !strings.HasPrefix(model, "claude-") {
		return 0, nil
	}
	var images []claudeImagePath
	blocks := 0
	var visit func(ojson.Result, string)
	visit = func(content ojson.Result, path string) {
		for i, block := range content.Array() {
			p := path + "." + strconv.Itoa(i)
			switch block.Get("type").String() {
			case "image":
				blocks++
				if source := block.Get("source"); source.Get("type").String() == "base64" {
					images = append(images, claudeImagePath{p + ".source", source})
				}
			case "document":
				blocks++
				visit(block.Get("source.content"), p+".source.content")
			case "tool_result":
				visit(block.Get("content"), p+".content")
			}
		}
	}
	for i, message := range doc.Get("messages").Array() {
		visit(message.Get("content"), "messages."+strconv.Itoa(i)+".content")
	}
	edge := 8000
	if blocks > 20 {
		edge = 3000
	}
	changed := 0
	for _, item := range images {
		data := item.source.Get("data").String()
		reader := func() *strings.Reader { return strings.NewReader(data) }
		cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, reader()))
		if err != nil || cfg.Width <= edge && cfg.Height <= edge {
			continue
		}
		key := claudeImageCacheKey{sha256.Sum256([]byte(data)), edge}
		claudeImageResizeCache.Lock()
		resized, ok := claudeImageResizeCache.items[key]
		claudeImageResizeCache.Unlock()
		if !ok {
			img, format, errDecode := image.Decode(base64.NewDecoder(base64.StdEncoding, reader()))
			if errDecode != nil {
				return changed, fmt.Errorf("resize Claude image at %s: invalid image", item.path)
			}
			width, height := cfg.Width, cfg.Height
			if width >= height {
				height = max(1, int(int64(height)*int64(edge)/int64(width)))
				width = edge
			} else {
				width = max(1, int(int64(width)*int64(edge)/int64(height)))
				height = edge
			}
			dst := image.NewRGBA(image.Rect(0, 0, width, height))
			xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
			var output bytes.Buffer
			resized.mediaType = "image/png"
			if format == "jpeg" {
				resized.mediaType = "image/jpeg"
				err = jpeg.Encode(&output, dst, &jpeg.Options{Quality: 95})
			} else {
				err = png.Encode(&output, dst)
			}
			if err != nil {
				return changed, fmt.Errorf("resize Claude image at %s: encoding failed", item.path)
			}
			resized.data = base64.StdEncoding.EncodeToString(output.Bytes())
			claudeImageResizeCache.Lock()
			if _, exists := claudeImageResizeCache.items[key]; !exists && len(resized.data) <= 32<<20 {
				for len(claudeImageResizeCache.order) >= 16 || claudeImageResizeCache.bytes+len(resized.data) > 32<<20 {
					old := claudeImageResizeCache.order[0]
					claudeImageResizeCache.order = claudeImageResizeCache.order[1:]
					claudeImageResizeCache.bytes -= len(claudeImageResizeCache.items[old].data)
					delete(claudeImageResizeCache.items, old)
				}
				claudeImageResizeCache.items[key] = resized
				claudeImageResizeCache.order = append(claudeImageResizeCache.order, key)
				claudeImageResizeCache.bytes += len(resized.data)
			}
			claudeImageResizeCache.Unlock()
		}
		if err = doc.Set(item.path+".data", resized.data); err != nil {
			return changed, err
		}
		if err = doc.Set(item.path+".media_type", resized.mediaType); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
