package webgui

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
)

const usageBlockBytes = 256 * 1024
const usageCacheBytes = 24 * 1024 * 1024
const usageCacheRows = 65536

type usageRecord struct {
	Row    billing.Row
	Offset int64
	Length int
}
type usageBlock struct {
	start, end int64
	digest     [32]byte
	rows       []usageRecord
	bytes      int
}
type usageProjectionCache struct {
	mu     sync.Mutex
	blocks []usageBlock
}
type pendingUsage struct {
	offset        int64
	start, length int
}

func projectionBytes(r billing.Row) int {
	// Include slice/struct overhead and owned strings; raw JSON is never retained.
	return 384 + len(r.Model) + len(r.KeyID) + len(r.ClientIP) + len(r.RequestID) + len(r.Path) + len(r.RoutedModel) + len(r.OAuthAccountID) + len(r.OAuthPrefix) + len(r.UpstreamReportedModel)
}

// visitUsage verifies every captured block's bytes before reusing its decoded
// projection. This handles append, same-inode edits, truncation and rotation,
// including out-of-order timestamps, without trusting append-only assumptions.
// At capacity it streams uncached blocks rather than retaining the entire log.
func (s *Server) visitUsage(visit func(usageRecord) error, finish func(*os.File) error) error {
	info, err := os.Lstat(s.options.Usage)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("usage unavailable")
	}
	file, err := os.Open(s.options.Usage)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("usage file changed")
	}
	cache := &s.usageCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	old := cache.blocks
	var next []usageBlock
	reader := bufio.NewReaderSize(io.NewSectionReader(file, 0, opened.Size()), 64*1024)
	hash := sha256.New()
	var start, offset int64
	var pending []pendingUsage
	var raw []byte
	var retainedBytes, retainedRows int
	cacheable := true
	oversized := false
	flush := func() error {
		block := usageBlock{start: start, end: offset}
		copy(block.digest[:], hash.Sum(nil))
		pos := len(next)
		if cacheable && pos < len(old) && old[pos].start == start && old[pos].end == offset && old[pos].digest == block.digest {
			block = old[pos]
		} else {
			for _, entry := range pending {
				row, err := billing.DecodeRow(raw[entry.start : entry.start+entry.length])
				if err != nil {
					continue
				}
				block.rows = append(block.rows, usageRecord{Row: row, Offset: entry.offset, Length: entry.length})
				block.bytes += projectionBytes(row)
			}
		}
		for _, entry := range block.rows {
			if err := visit(entry); err != nil {
				return err
			}
		}
		retainedBytes += block.bytes
		retainedRows += len(block.rows)
		if cacheable && retainedBytes <= usageCacheBytes && retainedRows <= usageCacheRows {
			next = append(next, block)
		} else {
			cacheable = false
			next = nil
		}
		start = offset
		hash.Reset()
		pending = pending[:0]
		raw = raw[:0]
		return nil
	}
	for {
		lineStart := offset
		line, readErr := reader.ReadSlice('\n')
		offset += int64(len(line))
		_, _ = hash.Write(line)
		if readErr == bufio.ErrBufferFull {
			oversized = true
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return errors.New("usage read failed")
		}
		if readErr == nil && !oversized {
			// Keep the original file offset separately from the packed raw offset.
			pending = append(pending, pendingUsage{start: len(raw), length: len(line), offset: lineStart})
			raw = append(raw, line...)
		}
		oversized = false
		if offset > start && (offset-start >= usageBlockBytes || readErr == io.EOF) {
			if err := flush(); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	current, err := os.Lstat(s.options.Usage)
	if err != nil || !os.SameFile(opened, current) || current.Size() < opened.Size() {
		return errors.New("usage file changed during read")
	}
	cache.blocks = next
	if finish != nil {
		return finish(file)
	}
	return nil
}
