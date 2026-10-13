package observer

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

const (
	ccMigrationSubsetModel = "commandcode/deepseek/deepseek-v4.1-flash"
	ccMigrationClaudeModel = "commandcode/claude-sonnet-4-5"
	ocMigrationModel       = "opencode-go/glm-5.2"
)

// legacyCommandCodeUnclassified reconstructs the request row a pre-fix writer
// persisted for a commandcode adapter record: raw counters exposed for display,
// the SDK's lower-bound total, and an unclassified quality.
func legacyCommandCodeUnclassified(seq uint64, id string, at time.Time, model string, raw pluginapi.UsageDetail) Request {
	ensured := usage.EnsureTokenBreakdownForProvider(usage.Detail{
		InputTokens: raw.InputTokens, OutputTokens: raw.OutputTokens,
		ReasoningTokens: raw.ReasoningTokens, CachedTokens: raw.CachedTokens,
		CacheReadTokens: raw.CacheReadTokens, CacheCreationTokens: raw.CacheCreationTokens,
		TotalTokens: raw.TotalTokens,
	}, ccProvider, ccExecutor)
	quality := string(ensured.TokenBreakdown.Quality)
	if quality == "" {
		quality = string(usage.TokenAccountingQualityUnclassified)
	}
	return Request{
		Sequence: seq, RequestID: id, TraceID: "trace-" + id, Time: at,
		Provider: ccProvider, Executor: ccExecutor, Model: model,
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

// migratedDBV1Accounting writes a database carrying the legacy stats/key-stats
// markers and the accounting marker at 1 (the opencode-go-only repair version),
// so Open must run the v1->v2 upgrade exactly once.
func migratedDBV1Accounting(t *testing.T, cfg Config, fn func(tx *bolt.Tx) error) {
	t.Helper()
	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketStats).SetSequence(1); err != nil {
			return err
		}
		if err := tx.Bucket(bucketKeyStats).SetSequence(1); err != nil {
			return err
		}
		if err := tx.Bucket(bucketAccountingMeta).SetSequence(1); err != nil {
			return err
		}
		return fn(tx)
	})
}

func putContribution(t *testing.T, tx *bolt.Tx, minute int64, r Request) error {
	t.Helper()
	if err := addRow(tx, bucketStats, statsKey(minute, r.Provider, r.Model), r); err != nil {
		return err
	}
	return addRow(tx, bucketKeyStats, keyStatsKey(minute, r.InputTokens, r.ClientKeyID, r.AuthIndex, r.Provider, r.Model), r)
}

func globalCounters(t *testing.T, s *Store, minute int64, provider, model string) Counters {
	t.Helper()
	var c Counters
	_ = s.db.View(func(tx *bolt.Tx) error {
		got, ok := decodeCounters(tx.Bucket(bucketStats).Get(statsKey(minute, provider, model)))
		if !ok {
			t.Fatalf("global stats missing for %s/%s", provider, model)
		}
		c = got
		return nil
	})
	return c
}

func assertPersistedEqual(t *testing.T, where string, got, want Counters) {
	t.Helper()
	if persistedWords(got) != persistedWords(want) {
		t.Fatalf("%s counters = %+v, want %+v", where, got, want)
	}
}

// TestCommandCodeAccountingMigrationV1ToV2 simulates a database already marked
// complete by the opencode-go-only v1 repair and verifies the v1->v2 upgrade
// additionally repairs only the commandcode cohort: recoverable subset and
// Anthropic rows are repaired with their raw counters preserved, the Anthropic
// input-length key moves, out-of-cohort and unrecoverable rows stay untouched,
// and already-complete rows are not double counted. Reopening is byte-stable.
func TestCommandCodeAccountingMigrationV1ToV2(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	minute := statsMinute(at)
	client := strings.Repeat("a", 64)
	auth := strings.Repeat("b", 16)

	// Recoverable subset: input length is unchanged by the repair.
	r1 := legacyCommandCodeUnclassified(1, "cc-sub", at, ccMigrationSubsetModel,
		pluginapi.UsageDetail{InputTokens: 10, CacheReadTokens: 2, OutputTokens: 5, TotalTokens: 15})
	r1.ClientKeyID, r1.AuthIndex = client, auth
	// Recoverable Anthropic: repaired input 100->170 moves its key contribution.
	r2 := legacyCommandCodeUnclassified(2, "cc-anth", at.Add(time.Second), ccMigrationClaudeModel,
		pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20, OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 370})
	r2.ClientKeyID, r2.AuthIndex = client, auth
	// Contradictory total: never recovered.
	r3 := legacyCommandCodeUnclassified(3, "cc-bad", at.Add(2*time.Second), ccMigrationSubsetModel,
		pluginapi.UsageDetail{InputTokens: 7, CacheReadTokens: 1, OutputTokens: 2, TotalTokens: 999})
	r3.ClientKeyID, r3.AuthIndex = client, auth
	// Out-of-cohort namespace on the same provider: never recovered.
	r4 := legacyCommandCodeUnclassified(4, "cc-out", at.Add(3*time.Second), "deepseek/deepseek-v4.1-flash",
		pluginapi.UsageDetail{InputTokens: 5, CacheReadTokens: 2, OutputTokens: 3, TotalTokens: 8})
	// Already complete commandcode and opencode rows: left byte-for-byte.
	r5 := makeRetainedRequest(5, "cc-done", at.Add(4*time.Second), ccProvider, ccMigrationSubsetModel, "complete", 50, 10, 5, 20, 0)
	r6 := makeRetainedRequest(6, "oc-done", at.Add(5*time.Second), "opencode-go", ocMigrationModel, "complete", 30, 5, 0, 10, 0)

	rows := []Request{r1, r2, r3, r4, r5, r6}
	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{
			ccMigrationSubsetModel: {Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1},
			ccMigrationClaudeModel: {Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1},
			ocMigrationModel:       {Input: 1, Output: 1},
		}
	})
	migratedDBV1Accounting(t, cfg, func(tx *bolt.Tx) error {
		for _, r := range rows {
			if err := putRequestRow(tx, r); err != nil {
				return err
			}
			if err := putContribution(t, tx, minute, r); err != nil {
				return err
			}
		}
		return nil
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Expected post-repair rows feed the expected aggregate contributions.
	r1Final := r1
	r1Final.AccountingQuality = "complete"
	r1Final.InputTokens, r1Final.UncachedInputTokens, r1Final.CacheReadTokens = 10, 8, 2
	r1Final.CacheCreationTokens, r1Final.OutputTokens, r1Final.ReasoningTokens, r1Final.TotalTokens = 0, 5, 0, 15
	r2Final := r2
	r2Final.AccountingQuality = "complete"
	r2Final.InputTokens, r2Final.UncachedInputTokens, r2Final.CacheReadTokens = 170, 100, 50
	r2Final.CacheCreationTokens, r2Final.OutputTokens, r2Final.ReasoningTokens, r2Final.TotalTokens = 20, 200, 100, 370

	// Global aggregates: only the commandcode subset/Anthropic rows changed;
	// the out-of-cohort row keeps its unclassified contribution.
	wantSubset := requestContribution(r1Final)
	addRequestCounters(&wantSubset, r3)
	addRequestCounters(&wantSubset, r5)
	assertPersistedEqual(t, "cc subset global",
		globalCounters(t, s, minute, ccProvider, ccMigrationSubsetModel), wantSubset)
	assertPersistedEqual(t, "cc claude global",
		globalCounters(t, s, minute, ccProvider, ccMigrationClaudeModel), requestContribution(r2Final))
	assertPersistedEqual(t, "out-of-cohort global",
		globalCounters(t, s, minute, ccProvider, "deepseek/deepseek-v4.1-flash"), requestContribution(r4))
	assertPersistedEqual(t, "opencode global",
		globalCounters(t, s, minute, "opencode-go", ocMigrationModel), requestContribution(r6))

	// Key partitions: the Anthropic contribution moved to the longer input key,
	// and the exact pre-repair key is drained.
	_ = s.db.View(func(tx *bolt.Tx) error {
		kb := tx.Bucket(bucketKeyStats)
		if kb.Get(keyStatsKey(minute, 100, client, auth, ccProvider, ccMigrationClaudeModel)) != nil {
			t.Fatalf("drained Anthropic input key retained")
		}
		moved, ok := decodeCounters(kb.Get(keyStatsKey(minute, 170, client, auth, ccProvider, ccMigrationClaudeModel)))
		if !ok {
			t.Fatalf("moved Anthropic input key missing")
		}
		assertPersistedEqual(t, "moved Anthropic key", moved, requestContribution(r2Final))
		kept, ok := decodeCounters(kb.Get(keyStatsKey(minute, 10, client, auth, ccProvider, ccMigrationSubsetModel)))
		if !ok {
			t.Fatalf("repaired subset key missing")
		}
		assertPersistedEqual(t, "repaired subset key", kept, requestContribution(r1Final))
		return nil
	})

	// Retained request rows: repaired rows complete, others untouched.
	byID := map[string]Request{}
	for _, got := range allRequests(t, s) {
		byID[got.RequestID] = got
	}
	got1 := byID["cc-sub"]
	if got1.AccountingQuality != "complete" || got1.InputTokens != 10 || got1.UncachedInputTokens != 8 ||
		got1.CacheReadTokens != 2 || got1.OutputTokens != 5 || got1.TotalTokens != 15 {
		t.Fatalf("subset row not repaired: %+v", got1)
	}
	if got1.RawUsage.InputTokens != 10 || got1.RawUsage.CacheReadTokens != 2 || got1.RawUsage.TotalTokens != 15 {
		t.Fatalf("subset raw usage changed: %+v", got1.RawUsage)
	}
	got2 := byID["cc-anth"]
	if got2.AccountingQuality != "complete" || got2.InputTokens != 170 || got2.UncachedInputTokens != 100 ||
		got2.CacheReadTokens != 50 || got2.CacheCreationTokens != 20 || got2.OutputTokens != 200 ||
		got2.ReasoningTokens != 100 || got2.TotalTokens != 370 {
		t.Fatalf("Anthropic row not repaired: %+v", got2)
	}
	if got2.RawUsage.InputTokens != 100 || got2.RawUsage.OutputTokens != 200 || got2.RawUsage.ReasoningTokens != 100 {
		t.Fatalf("Anthropic raw usage changed: %+v", got2.RawUsage)
	}
	if byID["cc-bad"].AccountingQuality != "unclassified" || byID["cc-out"].AccountingQuality != "unclassified" {
		t.Fatalf("unrecoverable rows changed: %+v / %+v", byID["cc-bad"], byID["cc-out"])
	}
	if byID["cc-done"].AccountingQuality != "complete" || byID["oc-done"].AccountingQuality != "complete" {
		t.Fatalf("already complete rows changed: %+v / %+v", byID["cc-done"], byID["oc-done"])
	}

	// Client and auth partitions expose the same complete basis.
	clientView, err := s.Summary(Query{Provider: ccProvider, ClientKeyID: client})
	if err != nil {
		t.Fatal(err)
	}
	if clientView.Totals.Requests != 3 || clientView.Totals.UnpricedRequests != 1 {
		t.Fatalf("client view = %+v, want 3 requests / 1 unpriced", clientView.Totals)
	}
	if ca := clientView.Totals.CacheAccounting; ca == nil ||
		ca.Requests != 2 || ca.InputTokens != 180 || ca.CacheReadTokens != 52 {
		t.Fatalf("client cache_accounting = %+v, want 2/180/52", ca)
	}
	authView, err := s.Summary(Query{Provider: ccProvider, AuthIndex: auth})
	if err != nil {
		t.Fatal(err)
	}
	if authView.Totals.Requests != 3 || authView.Totals.UnpricedRequests != 1 {
		t.Fatalf("auth view = %+v, want 3 requests / 1 unpriced", authView.Totals)
	}

	// Provider view counts all five same-provider rows: three complete, two
	// unpriced (unrecoverable in-cohort and out-of-cohort).
	providerView, err := s.Summary(Query{Provider: ccProvider})
	if err != nil {
		t.Fatal(err)
	}
	if providerView.Totals.Requests != 5 || providerView.Totals.UnpricedRequests != 2 {
		t.Fatalf("provider view = %+v, want 5 requests / 2 unpriced", providerView.Totals)
	}

	// Marker upgraded to the v2 value.
	_ = s.db.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != accountingMigrationSequence {
			t.Fatalf("accounting marker = %d, want %d", seq, accountingMigrationSequence)
		}
		return nil
	})

	// Reopen must not rescan or change any persisted byte and must not persist
	// derived cost.
	snapshot := map[string][]byte{}
	_ = s.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketStats, bucketKeyStats, bucketRequests, bucketAccountingMeta} {
			_ = tx.Bucket(name).ForEach(func(k, v []byte) error {
				snapshot[string(name)+"\x00"+string(k)] = append([]byte(nil), v...)
				return nil
			})
		}
		return nil
	})
	_ = s.Close()

	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	_ = s2.db.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != accountingMigrationSequence {
			t.Fatalf("reopened accounting marker = %d, want %d", seq, accountingMigrationSequence)
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
		if raw := tx.Bucket(bucketRequests).Get(encodeRequestKey(r1.Time, r1.Sequence)); strings.Contains(string(raw), "cost_usd") {
			t.Fatalf("derived cost persisted: %s", raw)
		}
		return nil
	})
}

// TestCommandCodeAccountingMigrationRollbackV1ToV2 injects corruption after a
// recoverable commandcode row and verifies the v1->v2 upgrade aborts atomically:
// the marker stays at 1, no request/global/key byte changes, and the bbolt lock
// is released so a repaired database can be reopened successfully.
func TestCommandCodeAccountingMigrationRollbackV1ToV2(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	minute := statsMinute(at)
	client := strings.Repeat("c", 64)

	good := legacyCommandCodeUnclassified(1, "cc-good", at, ccMigrationSubsetModel,
		pluginapi.UsageDetail{InputTokens: 10, CacheReadTokens: 2, OutputTokens: 5, TotalTokens: 15})
	good.ClientKeyID = client

	cfg := testConfig(t, nil)
	migratedDBV1Accounting(t, cfg, func(tx *bolt.Tx) error {
		if err := putRequestRow(tx, good); err != nil {
			return err
		}
		if err := putContribution(t, tx, minute, good); err != nil {
			return err
		}
		// A corrupt row after the recoverable one aborts the whole upgrade.
		return tx.Bucket(bucketRequests).Put(encodeRequestKey(at.Add(time.Second), 2), []byte("{"))
	})

	globalKey := statsKey(minute, ccProvider, ccMigrationSubsetModel)
	keyKey := keyStatsKey(minute, good.InputTokens, client, "", ccProvider, ccMigrationSubsetModel)
	var beforeGlobal, beforeKey, beforeRequest []byte
	db, err := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.View(func(tx *bolt.Tx) error {
		beforeGlobal = append([]byte(nil), tx.Bucket(bucketStats).Get(globalKey)...)
		beforeKey = append([]byte(nil), tx.Bucket(bucketKeyStats).Get(keyKey)...)
		beforeRequest = append([]byte(nil), tx.Bucket(bucketRequests).Get(encodeRequestKey(good.Time, good.Sequence))...)
		return nil
	})
	_ = db.Close()

	s, err := Open(cfg)
	if err == nil {
		_ = s.Close()
		t.Fatalf("expected Open to fail on corrupt retained request")
	}
	if !strings.Contains(err.Error(), "corrupt retained request") {
		t.Fatalf("error = %v, want corrupt retained request", err)
	}

	// Rollback: marker still 1, aggregates/request unchanged, lock released.
	db2, openErr := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
	if openErr != nil {
		t.Fatalf("bbolt lock not released: %v", openErr)
	}
	_ = db2.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != 1 {
			t.Errorf("accounting marker = %d, want 1 after rollback", seq)
		}
		if got := tx.Bucket(bucketStats).Get(globalKey); string(got) != string(beforeGlobal) {
			t.Errorf("global stats changed despite rollback")
		}
		if got := tx.Bucket(bucketKeyStats).Get(keyKey); string(got) != string(beforeKey) {
			t.Errorf("key stats changed despite rollback")
		}
		if got := tx.Bucket(bucketRequests).Get(encodeRequestKey(good.Time, good.Sequence)); string(got) != string(beforeRequest) {
			t.Errorf("repaired request changed despite rollback")
		}
		return nil
	})
	// Repair the corrupt row in place: replace it with a valid, non-target row.
	if err := db2.Update(func(tx *bolt.Tx) error {
		bad := makeRetainedRequest(2, "fixed", at.Add(time.Second), "mystery", "mystery-model", "unclassified", 1, 0, 0, 1, 0)
		return putRequestRow(tx, bad)
	}); err != nil {
		t.Fatalf("repair corrupt row: %v", err)
	}
	_ = db2.Close()

	s3, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open after repair failed: %v", err)
	}
	defer s3.Close()
	_ = s3.db.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != accountingMigrationSequence {
			t.Fatalf("marker after repair = %d, want %d", seq, accountingMigrationSequence)
		}
		return nil
	})
	for _, got := range allRequests(t, s3) {
		if got.RequestID == "cc-good" && got.AccountingQuality != "complete" {
			t.Fatalf("commandcode row not repaired after corruption fixed: %+v", got)
		}
	}
}

// TestAccountingMigrationDoesNotDowngradeFutureMarker verifies the completion
// guard is monotonic: a database written by a newer version keeps its marker and
// is not rescanned or downgraded.
func TestAccountingMigrationDoesNotDowngradeFutureMarker(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	minute := statsMinute(at)
	r := legacyCommandCodeUnclassified(1, "cc", at, ccMigrationSubsetModel,
		pluginapi.UsageDetail{InputTokens: 5, CacheReadTokens: 2, OutputTokens: 3, TotalTokens: 8})

	cfg := testConfig(t, nil)
	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketStats).SetSequence(1); err != nil {
			return err
		}
		if err := tx.Bucket(bucketKeyStats).SetSequence(1); err != nil {
			return err
		}
		if err := tx.Bucket(bucketAccountingMeta).SetSequence(3); err != nil {
			return err
		}
		if err := putRequestRow(tx, r); err != nil {
			return err
		}
		return putContribution(t, tx, minute, r)
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()
	_ = s.db.View(func(tx *bolt.Tx) error {
		if seq := tx.Bucket(bucketAccountingMeta).Sequence(); seq != 3 {
			t.Fatalf("future marker downgraded to %d, want 3", seq)
		}
		return nil
	})
	for _, got := range allRequests(t, s) {
		if got.RequestID == "cc" && got.AccountingQuality != "unclassified" {
			t.Fatalf("future-marker database was rescanned: %+v", got)
		}
	}
}
