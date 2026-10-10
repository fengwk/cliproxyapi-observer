package observer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

// legacyUnclassified reconstructs the request row the pre-fix writer persisted
// for an opencode-go adapter record: raw counters exposed for display, the
// SDK's lower-bound total, and an unclassified quality.
func legacyUnclassified(seq uint64, id string, at time.Time, model string, raw pluginapi.UsageDetail) Request {
	ensured := usage.EnsureTokenBreakdownForProvider(usage.Detail{
		InputTokens: raw.InputTokens, OutputTokens: raw.OutputTokens,
		ReasoningTokens: raw.ReasoningTokens, CachedTokens: raw.CachedTokens,
		CacheReadTokens: raw.CacheReadTokens, CacheCreationTokens: raw.CacheCreationTokens,
		TotalTokens: raw.TotalTokens,
	}, openCodeGoProvider, openCodeGoExecutor)
	quality := string(ensured.TokenBreakdown.Quality)
	if quality == "" {
		quality = string(usage.TokenAccountingQualityUnclassified)
	}
	return Request{
		Sequence: seq, RequestID: id, TraceID: "trace-" + id, Time: at,
		Provider: openCodeGoProvider, Executor: openCodeGoExecutor, Model: model,
		AccountingQuality:   quality,
		InputTokens:         positive(raw.InputTokens),
		OutputTokens:        positive(raw.OutputTokens),
		ReasoningTokens:     positive(raw.ReasoningTokens),
		CacheReadTokens:     positive(raw.CacheReadTokens),
		CacheCreationTokens: positive(raw.CacheCreationTokens),
		TotalTokens:         ensured.TokenBreakdown.TotalTokens,
		RawUsage:            raw,
		LatencyNS:           int64(2 * time.Second),
		TTFTNS:              int64(time.Second),
		GenerationNS:        int64(time.Second),
		CacheHit:            raw.CacheReadTokens > 0,
	}
}

// migratedDB writes a database that already carries the legacy stats/key-stats
// markers, so only the accounting repair migration runs on Open.
func migratedDB(t *testing.T, cfg Config, fn func(tx *bolt.Tx) error) {
	t.Helper()
	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketStats).SetSequence(1); err != nil {
			return err
		}
		if err := tx.Bucket(bucketKeyStats).SetSequence(1); err != nil {
			return err
		}
		return fn(tx)
	})
}

func putRequestRow(tx *bolt.Tx, r Request) error {
	data, err := marshalStoredRequest(r)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketRequests).Put(encodeRequestKey(r.Time, r.Sequence), data)
}

func addRow(tx *bolt.Tx, bucket []byte, key []byte, r Request) error {
	b := tx.Bucket(bucket)
	var c Counters
	if v := b.Get(key); v != nil {
		decoded, ok := decodeCounters(v)
		if !ok {
			return errCorruptFixture
		}
		c = decoded
	}
	addRequestCounters(&c, r)
	return b.Put(key, encodeCounters(c))
}

var errCorruptFixture = errors.New("corrupt fixture")

// TestAccountingMigrationMixedRetained verifies the historical repair keeps
// expired unknown aggregates untouched, repairs only uniquely recoverable
// retained rows, and keeps global, key, client and auth views consistent.
func TestAccountingMigrationMixedRetained(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	minute := statsMinute(at)
	const model = "opencode-go/glm-5.2"
	client := strings.Repeat("a", 64)
	auth := strings.Repeat("b", 16)

	r1 := legacyUnclassified(1, "r1", at, model, pluginapi.UsageDetail{InputTokens: 5, CacheReadTokens: 2, OutputTokens: 3, TotalTokens: 8})
	r2 := legacyUnclassified(2, "r2", at.Add(time.Second), model, pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 4, TotalTokens: 14})
	// Contradictory total: retained but not uniquely recoverable.
	r3 := legacyUnclassified(3, "r3", at.Add(2*time.Second), model, pluginapi.UsageDetail{InputTokens: 7, CacheReadTokens: 1, OutputTokens: 2, TotalTokens: 999})
	r1.ClientKeyID, r1.AuthIndex = client, auth
	r3.ClientKeyID, r3.AuthIndex = client, auth
	// Already complete: must be left alone.
	r4 := makeRetainedRequest(4, "r4", at.Add(3*time.Second), openCodeGoProvider, model, "complete", 50, 10, 5, 20, 0)
	// Expired unknown: only its aggregate remains, no retained request row.
	expired := legacyUnclassified(0, "", at, model, pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 40, TotalTokens: 140})
	expired.ClientKeyID, expired.AuthIndex = client, auth

	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1}}
	})
	migratedDB(t, cfg, func(tx *bolt.Tx) error {
		for _, r := range []Request{r1, r2, r3, r4} {
			if err := putRequestRow(tx, r); err != nil {
				return err
			}
		}
		globalKey := statsKey(minute, openCodeGoProvider, model)
		for _, r := range []Request{r1, r2, r3, r4, expired} {
			if err := addRow(tx, bucketStats, globalKey, r); err != nil {
				return err
			}
			key := keyStatsKey(minute, r.InputTokens, r.ClientKeyID, r.AuthIndex, r.Provider, r.Model)
			if err := addRow(tx, bucketKeyStats, key, r); err != nil {
				return err
			}
		}
		return nil
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// Global aggregates.
	var global Counters
	_ = s.db.View(func(tx *bolt.Tx) error {
		var ok bool
		global, ok = decodeCounters(tx.Bucket(bucketStats).Get(statsKey(minute, openCodeGoProvider, model)))
		if !ok {
			t.Fatal("global stats missing")
		}
		return nil
	})
	if global.Requests != 5 || global.completeRequests != 3 {
		t.Fatalf("global requests/complete = %d/%d, want 5/3", global.Requests, global.completeRequests)
	}
	if global.InputTokens != 187 || global.OutputTokens != 69 || global.CacheReadTokens != 13 ||
		global.CacheCreationTokens != 5 || global.TotalTokens != 1246 {
		t.Fatalf("global tokens changed unexpectedly: %+v", global)
	}
	if global.billableInput != 63 || global.billableRead != 12 || global.billableWrite != 5 || global.billableOutput != 27 {
		t.Fatalf("global billable wrong: in=%d read=%d write=%d out=%d",
			global.billableInput, global.billableRead, global.billableWrite, global.billableOutput)
	}

	// Only r1 and r2 were repaired; r3 stays unclassified.
	byID := map[string]Request{}
	for _, r := range allRequests(t, s) {
		byID[r.RequestID] = r
	}
	repaired := byID["r1"]
	if repaired.AccountingQuality != "complete" || repaired.UncachedInputTokens != 3 ||
		repaired.CacheReadTokens != 2 || repaired.OutputTokens != 3 || repaired.TotalTokens != 8 {
		t.Fatalf("r1 not repaired correctly: %+v", repaired)
	}
	if byID["r2"].AccountingQuality != "complete" || byID["r2"].UncachedInputTokens != 10 {
		t.Fatalf("r2 not repaired correctly: %+v", byID["r2"])
	}
	if r3r := byID["r3"]; r3r.AccountingQuality != "unclassified" || r3r.TotalTokens != 999 {
		t.Fatalf("r3 must stay unclassified with its stored total: %+v", r3r)
	}
	if byID["r4"].AccountingQuality != "complete" {
		t.Fatalf("r4 changed: %+v", byID["r4"])
	}

	// Unfiltered summary uses global stats and prices 3 complete requests.
	sum, err := s.Summary(Query{Provider: openCodeGoProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	wantCost := (63.0 + 12.0 + 5.0 + 27.0) / 1_000_000
	if !approx(sum.Totals.CostUSD, wantCost) || sum.Totals.UnpricedRequests != 2 {
		t.Fatalf("global summary wrong: cost=%v unpriced=%d", sum.Totals.CostUSD, sum.Totals.UnpricedRequests)
	}

	// Known client view (from key stats) sees r1 repaired, r3 and the expired
	// unknown aggregate still unpriced.
	known, err := s.Summary(Query{Provider: openCodeGoProvider, ClientKeyID: client})
	if err != nil {
		t.Fatal(err)
	}
	if known.Totals.Requests != 3 || known.Totals.UnpricedRequests != 2 {
		t.Fatalf("known client view wrong: %+v", known.Totals)
	}
	// Unknown identity view aggregates the two complete rows.
	unknown, err := s.Summary(Query{Provider: openCodeGoProvider, ClientKeyID: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Totals.Requests != 2 || unknown.Totals.UnpricedRequests != 0 {
		t.Fatalf("unknown client view wrong: %+v", unknown.Totals)
	}
}

// TestAccountingMigrationMovesAnthropicInputKey verifies the Anthropic input
// normalization moves the retained contribution to the longer input-length key
// and that price tiers follow the repaired total.
func TestAccountingMigrationMovesAnthropicInputKey(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	minute := statsMinute(at)
	const model = "opencode-go/claude-sonnet-4-5"
	client := strings.Repeat("c", 64)

	r := legacyUnclassified(1, "r1", at, model, pluginapi.UsageDetail{
		InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20,
		OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 370,
	})
	r.ClientKeyID = client

	threshold := int64(150)
	fallback := Price{Input: 1, Output: 1}
	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{model: fallback}
		c.PriceRules = []PriceRule{{
			Model: model, InputTokensGT: &threshold,
			Price: Price{Input: 3, Output: 15, CacheRead: 2, CacheCreation: 2},
		}}
	})
	migratedDB(t, cfg, func(tx *bolt.Tx) error {
		if err := putRequestRow(tx, r); err != nil {
			return err
		}
		if err := tx.Bucket(bucketBodies).Put([]byte("body-1"), []byte(`{"model":"x"}`)); err != nil {
			return err
		}
		if err := addRow(tx, bucketStats, statsKey(minute, openCodeGoProvider, model), r); err != nil {
			return err
		}
		return addRow(tx, bucketKeyStats, keyStatsKey(minute, r.InputTokens, client, "", openCodeGoProvider, model), r)
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// Bodies are never touched by the accounting repair.
	_ = s.db.View(func(tx *bolt.Tx) error {
		if got := string(tx.Bucket(bucketBodies).Get([]byte("body-1"))); got != `{"model":"x"}` {
			t.Fatalf("body changed: %q", got)
		}
		return nil
	})

	var repaired Request
	for _, got := range allRequests(t, s) {
		if got.RequestID == "r1" {
			repaired = got
		}
	}
	if repaired.AccountingQuality != "complete" || repaired.InputTokens != 170 ||
		repaired.UncachedInputTokens != 100 || repaired.CacheReadTokens != 50 ||
		repaired.CacheCreationTokens != 20 || repaired.OutputTokens != 200 ||
		repaired.ReasoningTokens != 100 || repaired.TotalTokens != 370 {
		t.Fatalf("anthropic record not repaired: %+v", repaired)
	}
	// Raw usage, identities and the request/trace IDs are preserved.
	if repaired.RawUsage.InputTokens != 100 || repaired.RawUsage.OutputTokens != 200 ||
		repaired.ClientKeyID != client || repaired.RequestID != "r1" || repaired.TraceID != "trace-r1" {
		t.Fatalf("identity/raw fields changed: %+v", repaired)
	}

	// Global row reflects the longer authoritative total.
	_ = s.db.View(func(tx *bolt.Tx) error {
		g, ok := decodeCounters(tx.Bucket(bucketStats).Get(statsKey(minute, openCodeGoProvider, model)))
		if !ok {
			t.Fatal("global missing")
		}
		if g.InputTokens != 170 || g.TotalTokens != 370 || g.completeRequests != 1 {
			t.Fatalf("global not reconciled: %+v", g)
		}
		if g.billableInput != 100 || g.billableRead != 50 || g.billableWrite != 20 || g.billableOutput != 200 {
			t.Fatalf("global billable wrong: %+v", g)
		}
		oldKey := keyStatsKey(minute, 100, client, "", openCodeGoProvider, model)
		if tx.Bucket(bucketKeyStats).Get(oldKey) != nil {
			t.Fatalf("drained old input key row was retained")
		}
		newRow, ok := decodeCounters(tx.Bucket(bucketKeyStats).Get(keyStatsKey(minute, 170, client, "", openCodeGoProvider, model)))
		if !ok || newRow.Requests != 1 || newRow.completeRequests != 1 {
			t.Fatalf("new input key missing contribution: %+v", newRow)
		}
		return nil
	})

	// Price rule (threshold 150) applies because the repaired input total is 170.
	sum, err := s.Summary(Query{Provider: openCodeGoProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	want := (100.0*3 + 50.0*2 + 20.0*2 + 200.0*15) / 1_000_000
	if !approx(sum.Totals.CostUSD, want) {
		t.Fatalf("cost = %v, want %v", sum.Totals.CostUSD, want)
	}
	if sum.Totals.UnpricedRequests != 0 {
		t.Fatalf("unpriced = %d, want 0", sum.Totals.UnpricedRequests)
	}
}

// TestAccountingMigrationIdempotentAndNoCost verifies reopening neither rescans
// nor changes persisted bytes, and that no derived cost is written to disk.
func TestAccountingMigrationIdempotentAndNoCost(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	const model = "opencode-go/glm-5.2"
	r := legacyUnclassified(1, "r1", at, model, pluginapi.UsageDetail{InputTokens: 5, CacheReadTokens: 2, OutputTokens: 3, TotalTokens: 8})

	cfg := testConfig(t, nil)
	migratedDB(t, cfg, func(tx *bolt.Tx) error {
		if err := putRequestRow(tx, r); err != nil {
			return err
		}
		if err := addRow(tx, bucketStats, statsKey(statsMinute(at), openCodeGoProvider, model), r); err != nil {
			return err
		}
		return addRow(tx, bucketKeyStats, keyStatsKey(statsMinute(at), r.InputTokens, "", "", openCodeGoProvider, model), r)
	})

	s1, err := Open(cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	snapshot := map[string][]byte{}
	_ = s1.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketStats, bucketKeyStats, bucketRequests, bucketAccountingMeta} {
			_ = tx.Bucket(name).ForEach(func(k, v []byte) error {
				snapshot[string(name)+"\x00"+string(k)] = append([]byte(nil), v...)
				return nil
			})
		}
		return nil
	})
	_ = s1.Close()

	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	_ = s2.db.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != accountingMigrationSequence {
			t.Fatalf("accounting marker = %d, want %d", seq, accountingMigrationSequence)
		}
		for _, name := range [][]byte{bucketStats, bucketKeyStats, bucketRequests} {
			_ = tx.Bucket(name).ForEach(func(k, v []byte) error {
				key := string(name) + "\x00" + string(k)
				if string(snapshot[key]) != string(v) {
					t.Fatalf("bucket %s key %x changed on reopen", name, k)
				}
				return nil
			})
		}
		return nil
	})

	// No persisted cost, and the reopened row reprices at query time.
	var raw []byte
	_ = s2.db.View(func(tx *bolt.Tx) error {
		raw = append([]byte(nil), tx.Bucket(bucketRequests).Get(encodeRequestKey(r.Time, r.Sequence))...)
		return nil
	})
	if strings.Contains(string(raw), "cost_usd") {
		t.Fatalf("derived cost persisted: %s", raw)
	}
	if got := allRequests(t, s2); len(got) != 1 || got[0].AccountingQuality != "complete" {
		t.Fatalf("reopened request wrong: %+v", got)
	}
}

// TestAccountingMigrationCorruptionRollsBack verifies a malformed retained
// record aborts the whole migration atomically.
func TestAccountingMigrationCorruptionRollsBack(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	const model = "opencode-go/glm-5.2"
	good := legacyUnclassified(1, "r1", at, model, pluginapi.UsageDetail{InputTokens: 5, CacheReadTokens: 2, OutputTokens: 3, TotalTokens: 8})

	cases := []struct {
		name        string
		setup       func(tx *bolt.Tx) error
		errContains string
	}{
		{
			name: "corrupt_json",
			setup: func(tx *bolt.Tx) error {
				return tx.Bucket(bucketRequests).Put(encodeRequestKey(at, 9), []byte("{"))
			},
			errContains: "corrupt retained request",
		},
		{
			name: "bad_key_length",
			setup: func(tx *bolt.Tx) error {
				return tx.Bucket(bucketRequests).Put([]byte("short"), []byte("{}"))
			},
			errContains: "corrupt retained request key",
		},
		{
			name: "key_mismatch",
			setup: func(tx *bolt.Tx) error {
				data, _ := json.Marshal(good)
				return tx.Bucket(bucketRequests).Put(encodeRequestKey(at, 99), data)
			},
			errContains: "retained request key mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, nil)
			migratedDB(t, cfg, func(tx *bolt.Tx) error {
				if err := addRow(tx, bucketStats, statsKey(statsMinute(at), openCodeGoProvider, model), good); err != nil {
					return err
				}
				if err := addRow(tx, bucketKeyStats, keyStatsKey(statsMinute(at), good.InputTokens, "", "", openCodeGoProvider, model), good); err != nil {
					return err
				}
				return tc.setup(tx)
			})
			var beforeGlobal, beforeKey []byte
			db, err := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_ = db.View(func(tx *bolt.Tx) error {
				beforeGlobal = append([]byte(nil), tx.Bucket(bucketStats).Get(statsKey(statsMinute(at), openCodeGoProvider, model))...)
				beforeKey = append([]byte(nil), tx.Bucket(bucketKeyStats).Get(keyStatsKey(statsMinute(at), good.InputTokens, "", "", openCodeGoProvider, model))...)
				return nil
			})
			_ = db.Close()

			s, err := Open(cfg)
			if err == nil {
				_ = s.Close()
				t.Fatalf("expected Open error containing %q", tc.errContains)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("error = %v, want containing %q", err, tc.errContains)
			}
			// Rollback: marker cleared, aggregates untouched, lock released.
			db2, openErr := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
			if openErr != nil {
				t.Fatalf("bbolt lock not released: %v", openErr)
			}
			defer db2.Close()
			_ = db2.View(func(tx *bolt.Tx) error {
				if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != 0 {
					t.Errorf("accounting marker = %d, want 0 after rollback", seq)
				}
				if got := tx.Bucket(bucketStats).Get(statsKey(statsMinute(at), openCodeGoProvider, model)); string(got) != string(beforeGlobal) {
					t.Errorf("global stats changed despite rollback")
				}
				if got := tx.Bucket(bucketKeyStats).Get(keyStatsKey(statsMinute(at), good.InputTokens, "", "", openCodeGoProvider, model)); string(got) != string(beforeKey) {
					t.Errorf("key stats changed despite rollback")
				}
				return nil
			})
		})
	}
}

// TestAccountingMigrationSkipsUnsafe verifies conservative skipping when the
// exact aggregate rows are missing, saturated, or only legacy.
func TestAccountingMigrationSkipsUnsafe(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	const model = "opencode-go/glm-5.2"
	raw := pluginapi.UsageDetail{InputTokens: 5, CacheReadTokens: 2, OutputTokens: 3, TotalTokens: 8}

	cases := []struct {
		name  string
		setup func(tx *bolt.Tx, r Request) error
	}{
		{
			name: "global_row_missing",
			setup: func(tx *bolt.Tx, r Request) error {
				return addRow(tx, bucketKeyStats, keyStatsKey(statsMinute(at), r.InputTokens, "", "", openCodeGoProvider, model), r)
			},
		},
		{
			name: "only_legacy_unknown_key_row",
			setup: func(tx *bolt.Tx, r Request) error {
				if err := addRow(tx, bucketStats, statsKey(statsMinute(at), openCodeGoProvider, model), r); err != nil {
					return err
				}
				return addRow(tx, bucketKeyStats, keyStatsKey(statsMinute(at), -1, "", "", openCodeGoProvider, model), r)
			},
		},
		{
			name: "global_billable_saturated",
			setup: func(tx *bolt.Tx, r Request) error {
				sb := tx.Bucket(bucketStats)
				key := statsKey(statsMinute(at), openCodeGoProvider, model)
				var c Counters
				addRequestCounters(&c, r)
				c.billableInput = math.MaxUint64
				if err := sb.Put(key, encodeCounters(c)); err != nil {
					return err
				}
				return addRow(tx, bucketKeyStats, keyStatsKey(statsMinute(at), r.InputTokens, "", "", openCodeGoProvider, model), r)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := legacyUnclassified(1, "r1", at, model, raw)
			cfg := testConfig(t, nil)
			migratedDB(t, cfg, func(tx *bolt.Tx) error {
				if err := putRequestRow(tx, r); err != nil {
					return err
				}
				return tc.setup(tx, r)
			})
			s, err := Open(cfg)
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			defer s.Close()
			for _, got := range allRequests(t, s) {
				if got.RequestID == "r1" && got.AccountingQuality != "unclassified" {
					t.Fatalf("unsafe record was repaired: %+v", got)
				}
			}
		})
	}
}

// TestAccountingMigrationRetainsMixedOldKeyRow verifies that when a moved
// Anthropic contribution shares its old input-length key with a record that
// stays unclassified, only the moved part is removed and the row is retained.
func TestAccountingMigrationRetainsMixedOldKeyRow(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	minute := statsMinute(at)
	const model = "opencode-go/claude-sonnet-4-5"
	client := strings.Repeat("d", 64)

	move := legacyUnclassified(1, "move", at, model, pluginapi.UsageDetail{
		InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20,
		OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 370,
	})
	move.ClientKeyID = client
	// Same old input length and identity, but not recoverable, so it stays.
	stay := legacyUnclassified(2, "stay", at.Add(time.Second), model, pluginapi.UsageDetail{
		InputTokens: 100, OutputTokens: 40, TotalTokens: 0,
	})
	stay.ClientKeyID = client

	cfg := testConfig(t, nil)
	migratedDB(t, cfg, func(tx *bolt.Tx) error {
		for _, r := range []Request{move, stay} {
			if err := putRequestRow(tx, r); err != nil {
				return err
			}
			if err := addRow(tx, bucketStats, statsKey(minute, openCodeGoProvider, model), r); err != nil {
				return err
			}
			key := keyStatsKey(minute, r.InputTokens, r.ClientKeyID, r.AuthIndex, r.Provider, r.Model)
			if err := addRow(tx, bucketKeyStats, key, r); err != nil {
				return err
			}
		}
		return nil
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	_ = s.db.View(func(tx *bolt.Tx) error {
		retained, ok := decodeCounters(tx.Bucket(bucketKeyStats).Get(keyStatsKey(minute, 100, client, "", openCodeGoProvider, model)))
		if !ok || retained.Requests != 1 || retained.completeRequests != 0 {
			t.Fatalf("mixed old key row not retained correctly: ok=%v %+v", ok, retained)
		}
		moved, ok := decodeCounters(tx.Bucket(bucketKeyStats).Get(keyStatsKey(minute, 170, client, "", openCodeGoProvider, model)))
		if !ok || moved.Requests != 1 || moved.completeRequests != 1 {
			t.Fatalf("moved contribution missing from new key: ok=%v %+v", ok, moved)
		}
		global, ok := decodeCounters(tx.Bucket(bucketStats).Get(statsKey(minute, openCodeGoProvider, model)))
		if !ok || global.Requests != 2 || global.completeRequests != 1 {
			t.Fatalf("global wrong: ok=%v %+v", ok, global)
		}
		return nil
	})
}

// TestAccountingMigrationLargePageSplit drives a few hundred retained rows
// through the streaming cursor so request rows span multiple pages and many
// in-place rewrites force reseeks. It proves no row is skipped or duplicated
// and that aggregates and summaries stay exact across a reopen.
func TestAccountingMigrationLargePageSplit(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	const model = "opencode-go/glm-5.2"
	const subCount, anthCount, badCount = 300, 60, 40
	const total = subCount + anthCount + badCount

	subsetRaw := pluginapi.UsageDetail{InputTokens: 10, CacheReadTokens: 2, OutputTokens: 5, TotalTokens: 15}
	anthRaw := pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20, OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 370}
	badRaw := pluginapi.UsageDetail{InputTokens: 7, CacheReadTokens: 1, OutputTokens: 2, TotalTokens: 999}

	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1}}
	})
	seq := uint64(0)
	migratedDB(t, cfg, func(tx *bolt.Tx) error {
		add := func(id string, raw pluginapi.UsageDetail) error {
			seq++
			r := legacyUnclassified(seq, id, at, model, raw)
			if err := putRequestRow(tx, r); err != nil {
				return err
			}
			if err := addRow(tx, bucketStats, statsKey(statsMinute(at), openCodeGoProvider, model), r); err != nil {
				return err
			}
			return addRow(tx, bucketKeyStats, keyStatsKey(statsMinute(at), r.InputTokens, "", "", openCodeGoProvider, model), r)
		}
		for i := 0; i < subCount; i++ {
			if err := add(fmt.Sprintf("sub-%d", i), subsetRaw); err != nil {
				return err
			}
		}
		for i := 0; i < anthCount; i++ {
			if err := add(fmt.Sprintf("anth-%d", i), anthRaw); err != nil {
				return err
			}
		}
		for i := 0; i < badCount; i++ {
			if err := add(fmt.Sprintf("bad-%d", i), badRaw); err != nil {
				return err
			}
		}
		return nil
	})

	check := func(stage string, s *Store) {
		t.Helper()
		records := allRequests(t, s)
		if len(records) != total {
			t.Fatalf("%s: retained records = %d, want %d (skipped/duplicated)", stage, len(records), total)
		}
		complete := 0
		for _, r := range records {
			switch r.AccountingQuality {
			case "complete":
				complete++
			case "unclassified":
			default:
				t.Fatalf("%s: unexpected quality %q", stage, r.AccountingQuality)
			}
		}
		if complete != subCount+anthCount {
			t.Fatalf("%s: complete = %d, want %d", stage, complete, subCount+anthCount)
		}
		var global Counters
		_ = s.db.View(func(tx *bolt.Tx) error {
			var ok bool
			global, ok = decodeCounters(tx.Bucket(bucketStats).Get(statsKey(statsMinute(at), openCodeGoProvider, model)))
			if !ok {
				t.Fatalf("%s: global stats missing", stage)
			}
			return nil
		})
		if global.Requests != total || global.completeRequests != subCount+anthCount {
			t.Fatalf("%s: global requests/complete = %d/%d", stage, global.Requests, global.completeRequests)
		}
		wantInput := int64(subCount*10 + anthCount*170 + badCount*7)
		wantOutput := int64(subCount*5 + anthCount*200 + badCount*2)
		wantTotal := int64(subCount*15 + anthCount*370 + badCount*999)
		if int64(global.InputTokens) != wantInput || int64(global.OutputTokens) != wantOutput ||
			int64(global.TotalTokens) != wantTotal {
			t.Fatalf("%s: global tokens = %d/%d/%d, want %d/%d/%d", stage,
				global.InputTokens, global.OutputTokens, global.TotalTokens, wantInput, wantOutput, wantTotal)
		}
		// Prices are all 1, so the summary is the sum of the four billable buckets.
		wantBillable := int64(subCount*8+anthCount*100) + int64(subCount*2+anthCount*50) +
			int64(anthCount*20) + int64(subCount*5+anthCount*200)
		sum, err := s.Summary(Query{Provider: openCodeGoProvider, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Totals.Requests != total || sum.Totals.UnpricedRequests != badCount {
			t.Fatalf("%s: summary requests/unpriced = %d/%d", stage, sum.Totals.Requests, sum.Totals.UnpricedRequests)
		}
		if !approx(sum.Totals.CostUSD, float64(wantBillable)/1_000_000) {
			t.Fatalf("%s: cost = %v, want %v", stage, sum.Totals.CostUSD, float64(wantBillable)/1_000_000)
		}
	}

	s1, err := Open(cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	check("first", s1)
	if fi, err := os.Stat(cfg.DataPath); err != nil {
		t.Fatalf("stat db: %v", err)
	} else if fi.Size() < 32<<10 {
		t.Fatalf("fixture db too small (%d bytes) to span bbolt pages", fi.Size())
	}
	snapshot := map[string][]byte{}
	_ = s1.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketRequests, bucketStats, bucketKeyStats} {
			_ = tx.Bucket(name).ForEach(func(k, v []byte) error {
				snapshot[string(name)+"\x00"+string(k)] = append([]byte(nil), v...)
				return nil
			})
		}
		return nil
	})
	_ = s1.Close()

	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	check("reopen", s2)
	_ = s2.db.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != accountingMigrationSequence {
			t.Fatalf("accounting marker = %d, want %d", seq, accountingMigrationSequence)
		}
		for _, name := range [][]byte{bucketRequests, bucketStats, bucketKeyStats} {
			_ = tx.Bucket(name).ForEach(func(k, v []byte) error {
				key := string(name) + "\x00" + string(k)
				if string(snapshot[key]) != string(v) {
					t.Fatalf("bucket %s key %x changed on reopen", name, k)
				}
				return nil
			})
		}
		return nil
	})
}
