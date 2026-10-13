//go:build linux

package quota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleSnapshot() Snapshot {
	u, _ := Project([]byte(`{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_at":2000000}}}`))
	return Snapshot{Checked: 1000000, Accounts: []Account{{ID: "one", Prefix: "codex", Workspace: "team", Source: OfficialUsageURL, Status: "ok", Updated: new(int64(1000000)), Quota: &u}, {ID: "two", Prefix: "codex2", Workspace: "team", Source: OfficialUsageURL, Status: "unavailable"}}}
}
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	s, err := OpenStore(t.Context(), dir, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}
func TestSnapshotPersistencePermissionsAndStaleMerge(t *testing.T) {
	s, dir := testStore(t)
	if err := s.Save(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(dir, snapshotName))
	previous, err := s.Previous()
	if err != nil || len(previous.Accounts) != 2 {
		t.Fatal("snapshot unavailable")
	}
	current := sampleSnapshot()
	current.Accounts[0].Status = "unavailable"
	current.Accounts[0].Quota = nil
	current.Accounts[0].Updated = nil
	current = CarryPrevious(current, previous)
	if err := s.Save(current); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, snapshotName))
	if before.Mode() != after.Mode() || after.Mode().Perm() != 0640 {
		t.Fatal("public permissions lost")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, snapshotName))
	if strings.Contains(string(raw), "access_token") {
		t.Fatal("credential leaked")
	}
}
func TestCorruptPreviousSnapshotIsNeverOverwritten(t *testing.T) {
	s, dir := testStore(t)
	path := filepath.Join(dir, snapshotName)
	before := []byte(`{"broken":`)
	_ = os.WriteFile(path, before, 0640)
	if _, err := s.Previous(); err == nil {
		t.Fatal("corrupt previous snapshot accepted")
	}
	if s.Save(sampleSnapshot()) == nil {
		t.Fatal("corrupt snapshot replaced")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("corrupt bytes altered")
	}
}
func TestHistoryExclusionsAndCorruptionDoNotLoseQuota(t *testing.T) {
	s, dir := testStore(t)
	raw, _ := json.Marshal(Exclusions{Intervals: []Interval{{AccountID: "one", Start: 999999, End: 1000001}}})
	_ = os.WriteFile(filepath.Join(dir, exclusionsName), raw, 0640)
	if err := s.AppendHistory(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, historyName)); !os.IsNotExist(err) {
		t.Fatal("excluded snapshot entered fitting history")
	}
	_ = os.WriteFile(filepath.Join(dir, exclusionsName), []byte(`{"intervals":[]}`), 0640)
	if err := s.AppendHistory(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, historyName))
	var row HistoryRow
	if json.Unmarshal(before, &row) != nil || row.ID != "one" || row.Used != 4 || !row.Rounded {
		t.Fatal("wrong history schema")
	}
	_ = os.WriteFile(filepath.Join(dir, exclusionsName), []byte(`{"intervals":[{"account_id":"one","start_ts":0,"end_ts":1}]}`), 0640)
	if err := s.Save(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	if s.AppendHistory(sampleSnapshot()) == nil {
		t.Fatal("invalid exclusion accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, historyName))
	if string(after) != string(before) {
		t.Fatal("historical data altered")
	}
}
func TestIncompleteHistoricalLineIsNotAltered(t *testing.T) {
	s, dir := testStore(t)
	path := filepath.Join(dir, historyName)
	_ = os.WriteFile(path, []byte(`{"partial":`), 0640)
	if err := s.AppendHistory(sampleSnapshot()); err == nil {
		t.Fatal("incomplete history extended")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"partial":` {
		t.Fatal("historical bytes changed")
	}
}
func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
func TestRotationPreservesExistingArchivesAndExactHistory(t *testing.T) {
	s, dir := testStore(t)
	path := filepath.Join(dir, historyName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("historical-prefix\n")
	_ = file.Truncate(historyLimit + 1)
	_, _ = file.WriteAt([]byte("\n"), historyLimit)
	_ = file.Close()
	sum := fileHash(t, path)
	old := path + ".1"
	_ = os.WriteFile(old, []byte("previous archive\n"), 0640)
	if err := s.AppendHistory(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + ".archive-*")
	if len(matches) != 1 || fileHash(t, matches[0]) != sum {
		t.Fatal("rotated history lost or changed")
	}
	raw, _ := os.ReadFile(old)
	if string(raw) != "previous archive\n" {
		t.Fatal("old archive overwritten")
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "wham/usage") {
		t.Fatal("fresh history absent")
	}
}
func TestQuotaFileSymlinkAndDirectorySubstitutionRejected(t *testing.T) {
	s, dir := testStore(t)
	other := t.TempDir()
	outside := filepath.Join(other, "untouched.json")
	_ = os.WriteFile(outside, []byte("untouched"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, snapshotName)); err != nil {
		t.Fatal(err)
	}
	if s.Save(sampleSnapshot()) == nil {
		t.Fatal("snapshot symlink followed")
	}
	raw, _ := os.ReadFile(outside)
	if string(raw) != "untouched" {
		t.Fatal("outside target altered")
	}
	_ = os.Remove(filepath.Join(dir, snapshotName))
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir); _ = os.Rename(moved, dir) })
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	if s.Save(sampleSnapshot()) == nil {
		t.Fatal("directory substitution accepted")
	}
	if _, err := os.Stat(filepath.Join(other, snapshotName)); !os.IsNotExist(err) {
		t.Fatal("wrote through replaced parent")
	}
}
func TestCollectorSingleWriterOwnership(t *testing.T) {
	_, dir := testStore(t)
	file, err := os.OpenFile(filepath.Join(dir, ".cpa-quota-collector.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
		t.Fatal("kernel did not enforce collector ownership")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if second, err := OpenStore(ctx, dir, uint32(os.Geteuid()), uint32(os.Getegid())); err == nil {
		_ = second.Close()
		t.Fatal("second collector acquired ownership")
	}
}
