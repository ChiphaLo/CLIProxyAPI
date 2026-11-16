package helps

import (
	"math"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
)

// ParseBridgeModelCooldown recognizes CPA's structured admission rejection, not
// an ordinary upstream 429. It never changes the inner account's cooldown state.
func ParseBridgeModelCooldown(body []byte) (*time.Duration, bool) {
	if !ojson.ValidBytes(body) {
		return nil, false
	}
	err := ojson.GetBytesNoCopy(body, "error")
	if err.Get("code").String() != "model_cooldown" {
		return nil, false
	}
	model := err.Get("model")
	seconds := err.Get("reset_seconds")
	if model.Type != ojson.String || strings.TrimSpace(model.String()) == "" || seconds.Type != ojson.Number {
		return nil, false
	}
	value := seconds.Float()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > (7*24*time.Hour+time.Hour).Seconds() || math.Trunc(value) != value {
		return nil, false
	}
	delay := time.Duration(value) * time.Second
	return &delay, true
}
