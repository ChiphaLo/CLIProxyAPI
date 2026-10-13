//go:build linux

package hosting

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func exitFixture() LegacyExit {
	return LegacyExit{Version: 1, ConfirmedAt: "2026-10-14T00:00:00Z", Processes: []LegacyProcess{{Role: "gateway", PID: 100, StartTicks: 123}, {Role: "oauth", PID: 101, StartTicks: 123}, {Role: "gui", PID: 102, StartTicks: 123}, {Role: "helper", PID: 103, StartTicks: 123}}}
}
func TestHostedLegacyProofRefusesLiveAmbiguousAndUnconfirmedWriters(t *testing.T) {
	missing := func(uint32) ([]byte, error) { return nil, os.ErrNotExist }
	if err := validateLegacyExit(exitFixture(), missing); err != nil {
		t.Fatal("exited identities rejected", err)
	}
	stat := func(start string) []byte {
		return []byte("100 (name with ) parens) S " + strings.Repeat("0 ", 18) + start + " 0")
	}
	for _, kind := range []string{"active", "unreadable", "truncated", "duplicate-pid", "duplicate-role", "missing-role", "bad-timestamp"} {
		t.Run(kind, func(t *testing.T) {
			p := exitFixture()
			read := missing
			switch kind {
			case "active":
				read = func(uint32) ([]byte, error) { return stat("123"), nil }
			case "unreadable":
				read = func(uint32) ([]byte, error) { return nil, errors.New("denied") }
			case "truncated":
				read = func(uint32) ([]byte, error) { return []byte("bad stat"), nil }
			case "duplicate-pid":
				p.Processes[1].PID = p.Processes[0].PID
			case "duplicate-role":
				p.Processes[1].Role = "gateway"
			case "missing-role":
				p.Processes = p.Processes[:3]
			case "bad-timestamp":
				p.ConfirmedAt = "yesterday"
			}
			if err := validateLegacyExit(p, read); err == nil {
				t.Fatal("unsafe handoff proof accepted")
			}
		})
	}
	if err := validateLegacyExit(exitFixture(), func(uint32) ([]byte, error) { return stat("999"), nil }); err != nil {
		t.Fatal("reused PID with a different process generation was treated as the old writer", err)
	}
}
