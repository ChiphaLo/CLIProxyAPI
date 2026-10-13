package gateway

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const maxLimitIdentities = 4096
const maxLimitSamples = 131072

type limitState struct {
	active int
	times  []time.Time
}

// AdmissionLimits preserves the legacy 120/IP/min, 32 global and current
// per-key RPM/concurrency policies. All counters survive identity hot reload.
// Tracking memory is bounded; exhausted bookkeeping fails closed, not open.
type AdmissionLimits struct {
	mu        sync.Mutex
	keys, ips map[string]*limitState
	samples   int
	lastSweep time.Time
	slots     chan struct{}
	now       func() time.Time
}

func NewAdmissionLimits(global int) *AdmissionLimits {
	if global < 1 || global > 64 {
		global = 32
	}
	return &AdmissionLimits{keys: map[string]*limitState{}, ips: map[string]*limitState{}, slots: make(chan struct{}, global), now: time.Now}
}

func (l *AdmissionLimits) trim(s *limitState, now time.Time) {
	i := 0
	for i < len(s.times) && !s.times[i].After(now.Add(-time.Minute)) {
		i++
	}
	l.samples -= i
	copy(s.times, s.times[i:])
	s.times = s.times[:len(s.times)-i]
}

func (l *AdmissionLimits) state(all map[string]*limitState, id string, now time.Time) *limitState {
	if now.Sub(l.lastSweep) >= 15*time.Second {
		for _, table := range []map[string]*limitState{l.keys, l.ips} {
			for id, s := range table {
				l.trim(s, now)
				if s.active == 0 && len(s.times) == 0 {
					delete(table, id)
				}
			}
		}
		l.lastSweep = now
	}
	if s := all[id]; s != nil {
		l.trim(s, now)
		return s
	}
	if id == "" || len(all) >= maxLimitIdentities {
		return nil
	}
	s := &limitState{}
	all[id] = s
	return s
}

func (l *AdmissionLimits) chargeIP(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := l.state(l.ips, ip, now)
	if s == nil || len(s.times) >= 120 || l.samples >= maxLimitSamples {
		return false
	}
	s.times = append(s.times, now)
	l.samples++
	return true
}

func (l *AdmissionLimits) Global() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path != "/health" && !l.chargeIP(c.ClientIP()) {
			rateAbort(c, "CPA client IP rate limit reached")
			return
		}
		select {
		case l.slots <- struct{}{}:
			defer func() { <-l.slots }()
		default:
			rateAbort(c, "CPA global concurrency limit reached")
			return
		}
		c.Next()
	}
}

func rateAbort(c *gin.Context, message string) {
	c.Header("Retry-After", "60")
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "rate_limited", "message": message}})
}

func (l *AdmissionLimits) AcquireKey(id string, rpm, concurrent int, charge bool) (func(), bool) {
	l.mu.Lock()
	now := l.now()
	s := l.state(l.keys, id, now)
	if s == nil || rpm < 1 || concurrent < 1 || s.active >= concurrent || charge && (len(s.times) >= rpm || l.samples >= maxLimitSamples) {
		l.mu.Unlock()
		return nil, false
	}
	s.active++
	if charge {
		s.times = append(s.times, now)
		l.samples++
	}
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { l.mu.Lock(); s.active--; l.mu.Unlock() }) }, true
}

func (l *AdmissionLimits) ChargeTurn(id, ip string, rpm, concurrent int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	k := l.state(l.keys, id, now)
	i := l.state(l.ips, ip, now)
	if k == nil || i == nil || rpm < 1 || concurrent < 1 || k.active > concurrent || len(k.times) >= rpm || len(i.times) >= 120 || l.samples+2 > maxLimitSamples {
		return false
	}
	k.times = append(k.times, now)
	i.times = append(i.times, now)
	l.samples += 2
	return true
}
