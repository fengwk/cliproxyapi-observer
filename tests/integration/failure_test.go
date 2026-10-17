//go:build integration

package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Real host failover changes upstream credentials, not the observer principal.
// Failed executions with no tokens cost zero; the successful execution costs 15.
func TestFailoverExecutionsAndTerminalFailures(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "failover"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarnessWith(t, hostOptions{TwoUpstreams: true})
			created := h.createKey(t, validSpec())
			acceptancePutPricing(t, h, acceptancePricing{Prices: []acceptancePrice{
				{Model: modelName, Rates: acceptanceRatesAll(1_000_000)},
				{Model: upstreamModel, Rates: acceptanceRatesAll(1_000_000)},
			}, Rules: []acceptanceRule{}})
			n := 1
			if terminal {
				n = 100
			}
			h.mock.failNextRequests(429, n)
			status, _ := h.infer(t, created.Secret, false)
			if !terminal && status != 200 {
				t.Fatalf("failover status %d", status)
			}
			if terminal && status != 429 {
				t.Fatalf("terminal status %d", status)
			}
			calls := h.mock.callsSnapshot()
			if len(calls) < 2 {
				t.Fatalf("expected distinct upstream attempts, got %d", len(calls))
			}
			if !terminal && (len(calls) != 2 || calls[0].Header.Get("Authorization") == calls[1].Header.Get("Authorization")) {
				t.Fatal("host did not fail over to another credential")
			}
			h.await(t, "all actual attempts ingested", func() bool { return len(h.page(t, "page_size=100").Items) == len(calls) })
			var cost int64
			var successes int
			ids := map[string]bool{}
			trace := ""
			for _, r := range h.page(t, "page_size=100").Items {
				if r.ID == "" || ids[r.ID] || r.TraceID == "" || r.KeyID != created.Key.ID {
					t.Fatalf("invalid execution attribution %+v", r)
				}
				ids[r.ID] = true
				if trace == "" {
					trace = r.TraceID
				}
				if r.TraceID != trace {
					t.Fatal("retry traces diverged")
				}
				cost += r.CostMicroUSD
				if r.Failed {
					if r.CostMicroUSD != 0 {
						t.Fatal("failed execution invented usage")
					}
				} else {
					successes++
					acceptanceAssertCost(t, r, 15, "manual")
				}
			}
			wantSuccess := 1
			wantCost := int64(15)
			if terminal {
				wantSuccess = 0
				wantCost = 0
			}
			if successes != wantSuccess || cost != wantCost {
				t.Fatalf("successes=%d cost=%d", successes, cost)
			}
			h.await(t, "failure releases slot", func() bool { return h.keys(t)[0].Active == 0 })
			h.mock.requireClean(t)
		})
	}
}

// A clean-slate install uses the new default and never opens/migrates the old
// database merely because it exists in the host's data directory.
func TestNoLegacyDefaultDatabase(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(data, "cliproxyapi-observer.db")
	sentinel := []byte("foreign legacy data: must remain byte-for-byte unchanged")
	if err := os.WriteFile(legacy, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	mock := newMockUpstream(t)
	t.Cleanup(mock.Close)
	h, err := startHost(t, mock, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	h.createKey(t, validSpec())
	h.Stop()
	old, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(old, sentinel) {
		t.Fatal("legacy default changed")
	}
	if info, err := os.Stat(h.dbPath); err != nil || info.Size() == 0 {
		t.Fatal("new default database missing")
	}
}

// An incompatible nonempty default DB is rejected without rewriting its bytes.
// A successful startup first isolates refusal from unrelated host/build errors.
func TestForeignDatabaseRejectedWithoutMutation(t *testing.T) {
	h := newHarness(t)
	h.createKey(t, validSpec())
	h.Stop()
	sentinel := []byte("nonempty incompatible database")
	if err := os.WriteFile(h.dbPath, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := startHost(t, h.mock, h.dir)
	if err == nil {
		restarted.Stop()
		t.Fatal("foreign nonempty database accepted")
	}
	stored, readErr := os.ReadFile(h.dbPath)
	if readErr != nil || !bytes.Equal(stored, sentinel) {
		t.Fatal("foreign database was mutated")
	}
}
