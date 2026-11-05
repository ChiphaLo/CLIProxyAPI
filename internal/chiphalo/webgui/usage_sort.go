package webgui

import (
	"cmp"
	"container/heap"
	"math"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/modelidentity"
)

func usageSort(query url.Values) (string, string) {
	field := strings.TrimSpace(query.Get("sort"))
	switch field {
	case "ts", "model", "key_id", "input", "cached", "output", "total", "cost", "duration_ms", "ttft_ms", "output_tps":
	default:
		field = "ts"
	}
	order := "desc"
	if query.Get("order") == "asc" {
		order = "asc"
	}
	return field, order
}

type rankedUsage struct {
	entry   usageRecord
	integer int64
	decimal float64
	text    string
	missing bool
}

func rankUsage(entry usageRecord, cost float64, field string) rankedUsage {
	rank := rankedUsage{entry: entry}
	row := entry.Row
	switch field {
	case "model":
		rank.text = modelidentity.Display(row.Model, row.UpstreamReportedModel)
		rank.missing = rank.text == ""
	case "key_id":
		rank.text, rank.missing = row.KeyID, row.KeyID == ""
	case "input":
		rank.integer = max(row.Input-row.Cached, 0)
	case "cached":
		rank.integer = row.Cached
	case "output":
		rank.integer = row.Output
	case "total":
		rank.integer = max(row.Input-row.Cached, 0) + row.Cached + row.Output
	case "cost":
		rank.decimal = cost
	case "duration_ms":
		rank.integer = row.DurationMS
		rank.missing = row.DurationMS <= 0 || row.DurationMS > 7*86400000
	case "ttft_ms":
		rank.integer, rank.missing = row.TTFTMS, row.TTFTMS <= 0
	case "output_tps":
		rank.decimal = row.OutputTPS
		rank.missing = row.DurationMS <= 0 || row.DurationMS > 7*86400000 || row.OutputTPS <= 0 || math.IsNaN(row.OutputTPS) || math.IsInf(row.OutputTPS, 0)
	default:
		rank.integer, rank.missing = row.TS, row.TS <= 0
	}
	return rank
}

func compareRankedUsage(a, b rankedUsage, descending bool) int {
	// Unavailable latency/throughput values remain last in either direction.
	if a.missing != b.missing {
		if a.missing {
			return 1
		}
		return -1
	}
	result := 0
	if !a.missing {
		result = cmp.Compare(a.text, b.text)
		if result == 0 {
			result = cmp.Compare(a.integer, b.integer)
		}
		if result == 0 {
			result = cmp.Compare(a.decimal, b.decimal)
		}
		if descending {
			result = -result
		}
	}
	if result == 0 {
		result = cmp.Compare(b.entry.Row.TS, a.entry.Row.TS)
	}
	if result == 0 {
		// Match the original stable file order when timestamps are identical.
		result = cmp.Compare(a.entry.Offset, b.entry.Offset)
	}
	return result
}

// Keep only the requested top N while visiting every matching record. The worst
// retained record is the heap root, avoiding an unbounded full-log sort or the
// previous per-row sort of the whole recent-call slice.
type recentUsageHeap struct {
	rows       []rankedUsage
	descending bool
}

func (h recentUsageHeap) Len() int { return len(h.rows) }
func (h recentUsageHeap) Less(i, j int) bool {
	return compareRankedUsage(h.rows[i], h.rows[j], h.descending) > 0
}
func (h recentUsageHeap) Swap(i, j int)   { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *recentUsageHeap) Push(value any) { h.rows = append(h.rows, value.(rankedUsage)) }
func (h *recentUsageHeap) Pop() any {
	last := len(h.rows) - 1
	value := h.rows[last]
	h.rows[last] = rankedUsage{}
	h.rows = h.rows[:last]
	return value
}
func (h *recentUsageHeap) add(rank rankedUsage, limit int) {
	if h.Len() < limit {
		heap.Push(h, rank)
	} else if compareRankedUsage(rank, h.rows[0], h.descending) < 0 {
		h.rows[0] = rank
		heap.Fix(h, 0)
	}
}
