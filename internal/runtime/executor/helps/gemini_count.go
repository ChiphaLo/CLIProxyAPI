package helps

import (
	"github.com/MoeForge/moego/ojson"
)

// NormalizeGeminiCountModel keeps an embedded GenerateContentRequest on the
// already selected upstream model. Public account prefixes are local routes,
// not Google model resources. Call before the final payload rule barrier.
func NormalizeGeminiCountModel(body []byte, model string) []byte {
	for _, path := range []string{"generateContentRequest", "generate_content_request"} {
		if object := ojson.GetBytes(body, path); object.IsObject() {
			updated, err := ojson.SetBytes(body, path+".model", "models/"+model)
			if err == nil {
				body = updated
			}
		}
	}
	return body
}
