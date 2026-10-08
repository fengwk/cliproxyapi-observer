package observer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// rawLegacyCounters holds the 15 64-bit words representing the v1 legacy counters layout
// (120 bytes total). Words 0-12 are usage/latency metrics, word 13 is UnpricedRequests,
// and word 14 is the IEEE 754 float64 bit pattern of CostUSD.
type rawLegacyCounters struct {
	Requests            uint64
	FailedRequests      uint64
	InputTokens         uint64
	OutputTokens        uint64
	ReasoningTokens     uint64
	CacheReadTokens     uint64
	CacheCreationTokens uint64
	TotalTokens         uint64
	CacheHits           uint64
	LatencyNS           uint64
	LatencySamples      uint64
	TTFTNS              uint64
	TTFTSamples         uint64
	UnpricedRequests    uint64
	CostUSD             float64
}

// encodeLegacyCountersBytes manually formats a 120-byte legacy counters buffer
// using explicit BigEndian words, completely independent of encodeCounters.
func encodeLegacyCountersBytes(c rawLegacyCounters) []byte {
	buf := make([]byte, legacyCountersSize)
	put := func(i int, v uint64) {
		binary.BigEndian.PutUint64(buf[i*8:], v)
	}
	put(0, c.Requests)
	put(1, c.FailedRequests)
	put(2, c.InputTokens)
	put(3, c.OutputTokens)
	put(4, c.ReasoningTokens)
	put(5, c.CacheReadTokens)
	put(6, c.CacheCreationTokens)
	put(7, c.TotalTokens)
	put(8, c.CacheHits)
	put(9, c.LatencyNS)
	put(10, c.LatencySamples)
	put(11, c.TTFTNS)
	put(12, c.TTFTSamples)
	put(13, c.UnpricedRequests)
	put(14, math.Float64bits(c.CostUSD))
	return buf
}

// setupLegacyDB initializes a raw bbolt database with all buckets and initial legacy records
// prior to opening the store with Open. The stats bucket sequence starts at 0.
func setupLegacyDB(t *testing.T, path string, fn func(tx *bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("setupLegacyDB open: %v", err)
	}
	defer db.Close()
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if fn != nil {
			return fn(tx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("setupLegacyDB update: %v", err)
	}
}

func makeRetainedRequest(seq uint64, id string, t time.Time, provider, model, quality string, uncached, read, write, output, reasoning int64) Request {
	input := uncached + read + write
	return Request{
		Sequence:            seq,
		RequestID:           id,
		TraceID:             "trace-" + id,
		Time:                t,
		Provider:            provider,
		Model:               model,
		AccountingQuality:   quality,
		InputTokens:         input,
		UncachedInputTokens: uncached,
		CacheReadTokens:     read,
		CacheCreationTokens: write,
		OutputTokens:        output,
		ReasoningTokens:     reasoning,
		TotalTokens:         input + output,
		LatencyNS:           int64(2 * time.Second),
		TTFTNS:              int64(time.Second),
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func targetTestTime() time.Time {
	return time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
}

// TestMigration_FullLegacyFixture tests requirement 1 & 4:
// Handcrafting a real v1 legacy fixture (120 bytes) with UnpricedRequests=0, Requests=2,
// InputTokens=100, OutputTokens=40, CacheReadTokens=30, CacheCreationTokens=20, CostUSD=0.5.
// Verifies that Open derives completeRequests=2, billableInput=50, billableRead=30,
// billableWrite=20, billableOutput=40, ignores legacy CostUSD, reprices at query time,
// and preserves all 13 original counters verbatim.
func TestMigration_FullLegacyFixture(t *testing.T) {
	targetTime := targetTestTime()
	targetMinute := statsMinute(targetTime)

	legacy := rawLegacyCounters{
		Requests:            2,
		FailedRequests:      1,
		InputTokens:         100,
		OutputTokens:        40,
		ReasoningTokens:     15,
		CacheReadTokens:     30,
		CacheCreationTokens: 20,
		TotalTokens:         140,
		CacheHits:           1,
		LatencyNS:           2500000000,
		LatencySamples:      2,
		TTFTNS:              800000000,
		TTFTSamples:         2,
		UnpricedRequests:    0,
		CostUSD:             0.5,
	}

	sk := statsKey(targetMinute, "openai", "gpt-5")
	cfg := testConfig(t, func(c *Config) {
		c.Prices = nil
	})

	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy))
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// 1. Assert raw DB layout, sequence marker and counter values
	err = s.db.View(func(tx *bolt.Tx) error {
		sb := tx.Bucket(bucketStats)
		if sb == nil {
			return fmt.Errorf("stats bucket missing")
		}
		if seq := sb.Sequence(); seq != 1 {
			t.Errorf("Sequence marker want 1, got %d", seq)
		}
		val := sb.Get(sk)
		if len(val) != countersSize {
			t.Fatalf("migrated counters length want %d, got %d", countersSize, len(val))
		}
		if val[0] != 1 {
			t.Errorf("migrated version byte want 1, got %d", val[0])
		}
		c, ok := decodeCounters(val)
		if !ok {
			t.Fatalf("decodeCounters returned false")
		}

		// Requirement 4: verbatim assertion of all original counters
		if c.Requests != 2 {
			t.Errorf("Requests want 2, got %d", c.Requests)
		}
		if c.FailedRequests != 1 {
			t.Errorf("FailedRequests want 1, got %d", c.FailedRequests)
		}
		if c.InputTokens != 100 {
			t.Errorf("InputTokens want 100, got %d", c.InputTokens)
		}
		if c.OutputTokens != 40 {
			t.Errorf("OutputTokens want 40, got %d", c.OutputTokens)
		}
		if c.ReasoningTokens != 15 {
			t.Errorf("ReasoningTokens want 15, got %d", c.ReasoningTokens)
		}
		if c.CacheReadTokens != 30 {
			t.Errorf("CacheReadTokens want 30, got %d", c.CacheReadTokens)
		}
		if c.CacheCreationTokens != 20 {
			t.Errorf("CacheCreationTokens want 20, got %d", c.CacheCreationTokens)
		}
		if c.TotalTokens != 140 {
			t.Errorf("TotalTokens want 140, got %d", c.TotalTokens)
		}
		if c.CacheHits != 1 {
			t.Errorf("CacheHits want 1, got %d", c.CacheHits)
		}
		if c.LatencyNS != 2500000000 {
			t.Errorf("LatencyNS want 2500000000, got %d", c.LatencyNS)
		}
		if c.LatencySamples != 2 {
			t.Errorf("LatencySamples want 2, got %d", c.LatencySamples)
		}
		if c.TTFTNS != 800000000 {
			t.Errorf("TTFTNS want 800000000, got %d", c.TTFTNS)
		}
		if c.TTFTSamples != 2 {
			t.Errorf("TTFTSamples want 2, got %d", c.TTFTSamples)
		}

		// Derived unexported billable fields
		if c.completeRequests != 2 {
			t.Errorf("completeRequests want 2, got %d", c.completeRequests)
		}
		if c.billableInput != 50 { // 100 - 30 - 20
			t.Errorf("billableInput want 50, got %d", c.billableInput)
		}
		if c.billableRead != 30 {
			t.Errorf("billableRead want 30, got %d", c.billableRead)
		}
		if c.billableWrite != 20 {
			t.Errorf("billableWrite want 20, got %d", c.billableWrite)
		}
		if c.billableOutput != 40 {
			t.Errorf("billableOutput want 40, got %d", c.billableOutput)
		}
		if c.UnpricedRequests != 0 {
			t.Errorf("UnpricedRequests want 0, got %d", c.UnpricedRequests)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("db.View failed: %v", err)
	}

	// 2. Query summary with no prices configured:
	// Legacy CostUSD=0.5 must be ignored.
	// With no price config, cost must be 0 and unpriced_requests must be 2.
	sumNoPrice, err := s.Summary(Query{From: targetTime.Add(-time.Minute), To: targetTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}
	if sumNoPrice.Totals.CostUSD != 0 {
		t.Errorf("Summary without prices CostUSD want 0, got %v", sumNoPrice.Totals.CostUSD)
	}
	if sumNoPrice.Totals.UnpricedRequests != 2 {
		t.Errorf("Summary without prices UnpricedRequests want 2, got %d", sumNoPrice.Totals.UnpricedRequests)
	}

	// 3. Reopen with configured price and verify recalculation
	if err := s.Close(); err != nil {
		t.Fatalf("s.Close failed: %v", err)
	}

	cfgWithPrice := testConfig(t, func(c *Config) {
		c.DataPath = cfg.DataPath
		c.Prices = map[string]Price{
			"gpt-5": {
				Input:         2.0, // $2 / MTok
				Output:        4.0, // $4 / MTok
				CacheRead:     0.5, // $0.5 / MTok
				CacheCreation: 1.0, // $1 / MTok
			},
		}
	})

	s2, err := Open(cfgWithPrice)
	if err != nil {
		t.Fatalf("Open with prices failed: %v", err)
	}
	defer s2.Close()

	sumWithPrice, err := s2.Summary(Query{From: targetTime.Add(-time.Minute), To: targetTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary with prices failed: %v", err)
	}
	// Expected cost calculation:
	// billableInput:  50 * 2.0 = 100
	// billableRead:   30 * 0.5 = 15
	// billableWrite:  20 * 1.0 = 20
	// billableOutput: 40 * 4.0 = 160
	// sum = 295 / 1,000,000 = 0.000295 USD
	wantCost := 0.000295
	if !almostEqual(sumWithPrice.Totals.CostUSD, wantCost) {
		t.Errorf("Summary CostUSD want %v, got %v", wantCost, sumWithPrice.Totals.CostUSD)
	}
	if sumWithPrice.Totals.UnpricedRequests != 0 {
		t.Errorf("Summary UnpricedRequests want 0, got %d", sumWithPrice.Totals.UnpricedRequests)
	}
}

// TestMigration_PartialRequestsMixed tests requirement 2:
// legacy UnpricedRequests > 0 with retained requests containing mixed accounting quality
// (some complete, some unknown) for the same model, as well as requests for another minute or model.
// Verifies only retained complete rows are billed, unknown remainder remains unpriced,
// completeRequests does not exceed old Requests, and records across minutes/models are not conflated.
func TestMigration_PartialRequestsMixed(t *testing.T) {
	t0 := targetTestTime()
	minute0 := statsMinute(t0)
	tNextMinute := t0.Add(time.Minute)

	legacy := rawLegacyCounters{
		Requests:            5,
		FailedRequests:      0,
		InputTokens:         500,
		OutputTokens:        200,
		ReasoningTokens:     20,
		CacheReadTokens:     50,
		CacheCreationTokens: 30,
		TotalTokens:         700,
		CacheHits:           1,
		LatencyNS:           1000000000,
		LatencySamples:      5,
		TTFTNS:              400000000,
		TTFTSamples:         5,
		UnpricedRequests:    2, // partial legacy: triggers recoverRows
		CostUSD:             1.234,
	}

	sk := statsKey(minute0, "openai", "gpt-5")
	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{
			"gpt-5": {
				Input:         10.0,
				Output:        20.0,
				CacheRead:     5.0,
				CacheCreation: 5.0,
			},
		}
	})

	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		// 1. Put legacy stats
		if err := tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy)); err != nil {
			return err
		}

		rb := tx.Bucket(bucketRequests)
		putReq := func(r Request) error {
			data, err := json.Marshal(r)
			if err != nil {
				return err
			}
			return rb.Put(encodeRequestKey(r.Time, r.Sequence), data)
		}

		// Req 1: minute 0, gpt-5, complete
		r1 := makeRetainedRequest(1, "r1", t0, "openai", "gpt-5", "complete", 60, 20, 10, 40, 5)
		if err := putReq(r1); err != nil {
			return err
		}

		// Req 2: minute 0, gpt-5, complete
		r2 := makeRetainedRequest(2, "r2", t0.Add(time.Second), "openai", "gpt-5", "complete", 50, 10, 0, 30, 0)
		if err := putReq(r2); err != nil {
			return err
		}

		// Req 3: minute 0, gpt-5, unknown accounting quality
		r3 := makeRetainedRequest(3, "r3", t0.Add(2*time.Second), "openai", "gpt-5", "unknown", 40, 10, 0, 20, 0)
		if err := putReq(r3); err != nil {
			return err
		}

		// Req 4: across minute (minute 1), gpt-5, complete -> should NOT be matched to minute0 stats
		r4 := makeRetainedRequest(4, "r4", tNextMinute, "openai", "gpt-5", "complete", 100, 0, 0, 50, 0)
		if err := putReq(r4); err != nil {
			return err
		}

		// Req 5: minute 0, different model ("gpt-4"), complete -> should NOT be matched to gpt-5 stats
		r5 := makeRetainedRequest(5, "r5", t0.Add(3*time.Second), "openai", "gpt-4", "complete", 80, 0, 0, 40, 0)
		if err := putReq(r5); err != nil {
			return err
		}

		return nil
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// Verify migrated counters in DB
	err = s.db.View(func(tx *bolt.Tx) error {
		val := tx.Bucket(bucketStats).Get(sk)
		c, ok := decodeCounters(val)
		if !ok {
			t.Fatalf("decodeCounters failed")
		}

		if c.Requests != 5 {
			t.Errorf("Requests want 5, got %d", c.Requests)
		}
		if c.InputTokens != 500 {
			t.Errorf("InputTokens want 500, got %d", c.InputTokens)
		}
		// Only r1 and r2 complete requests counted (2 complete rows out of 3 matched)
		if c.completeRequests != 2 {
			t.Errorf("completeRequests want 2, got %d", c.completeRequests)
		}
		// billableInput: 60 (r1) + 50 (r2) = 110
		if c.billableInput != 110 {
			t.Errorf("billableInput want 110, got %d", c.billableInput)
		}
		// billableRead: 20 (r1) + 10 (r2) = 30
		if c.billableRead != 30 {
			t.Errorf("billableRead want 30, got %d", c.billableRead)
		}
		// billableWrite: 10 (r1) + 0 (r2) = 10
		if c.billableWrite != 10 {
			t.Errorf("billableWrite want 10, got %d", c.billableWrite)
		}
		// billableOutput: 40 (r1) + 30 (r2) = 70
		if c.billableOutput != 70 {
			t.Errorf("billableOutput want 70, got %d", c.billableOutput)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("db.View failed: %v", err)
	}

	// Verify Summary pricing
	sum, err := s.Summary(Query{From: t0, To: t0.Add(time.Minute), Model: "gpt-5"})
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}
	if sum.Totals.Requests != 5 {
		t.Errorf("Summary Requests want 5, got %d", sum.Totals.Requests)
	}
	// 5 total requests - 2 complete = 3 unpriced
	if sum.Totals.UnpricedRequests != 3 {
		t.Errorf("Summary UnpricedRequests want 3, got %d", sum.Totals.UnpricedRequests)
	}
	// Cost calculation:
	// billableInput:  110 * 10 = 1100
	// billableRead:   30 * 5   = 150
	// billableWrite:  10 * 5   = 50
	// billableOutput: 70 * 20  = 1400
	// sum = 2700 / 1e6 = 0.0027 USD
	wantCost := 0.0027
	if !almostEqual(sum.Totals.CostUSD, wantCost) {
		t.Errorf("Summary CostUSD want %v, got %v", wantCost, sum.Totals.CostUSD)
	}
}

// TestMigration_PartialNoRequestsDetails tests requirement 3:
// partial legacy (UnpricedRequests > 0) but without any retained request rows in bucketRequests.
// Verifies completeRequests=0, billable counters=0, and unknown remainder stays fully unpriced.
func TestMigration_PartialNoRequestsDetails(t *testing.T) {
	targetTime := targetTestTime()
	targetMinute := statsMinute(targetTime)

	legacy := rawLegacyCounters{
		Requests:         3,
		InputTokens:      300,
		OutputTokens:     150,
		UnpricedRequests: 1, // partial legacy
		CostUSD:          0.8,
	}

	sk := statsKey(targetMinute, "openai", "gpt-5")
	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{
			"gpt-5": {Input: 5.0, Output: 10.0},
		}
	})

	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy))
	})

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// In DB, completeRequests and billable counters must be 0
	err = s.db.View(func(tx *bolt.Tx) error {
		val := tx.Bucket(bucketStats).Get(sk)
		c, ok := decodeCounters(val)
		if !ok {
			t.Fatalf("decodeCounters failed")
		}
		if c.completeRequests != 0 {
			t.Errorf("completeRequests want 0, got %d", c.completeRequests)
		}
		if c.billableInput != 0 || c.billableRead != 0 || c.billableWrite != 0 || c.billableOutput != 0 {
			t.Errorf("billable counters must be 0, got input=%d read=%d write=%d output=%d",
				c.billableInput, c.billableRead, c.billableWrite, c.billableOutput)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("db.View failed: %v", err)
	}

	// In Summary, all requests remain unpriced and cost is 0
	sum, err := s.Summary(Query{From: targetTime.Add(-time.Minute), To: targetTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}
	if sum.Totals.Requests != 3 {
		t.Errorf("Summary Requests want 3, got %d", sum.Totals.Requests)
	}
	if sum.Totals.UnpricedRequests != 3 {
		t.Errorf("Summary UnpricedRequests want 3, got %d", sum.Totals.UnpricedRequests)
	}
	if sum.Totals.CostUSD != 0 {
		t.Errorf("Summary CostUSD want 0, got %v", sum.Totals.CostUSD)
	}
}

// TestMigration_FailSafe tests requirement 5:
// fail-safe cases triggering migration errors:
// 1) illegal legacy length
// 2) malformed stats key
// 3) UnpricedRequests > Requests
// 4) complete request token arithmetic mismatch
// 5) request key does not match (Time, Sequence)
// 6) retained matched requests > legacy Requests
//
// For each case, asserts:
// - Open returns an error
// - The transaction rolled back: bucketStats.Sequence() remains 0
// - Stats raw bytes are unmodified
// - The failed Open released the bbolt file lock (bolt.Open succeeds)
func TestMigration_FailSafe(t *testing.T) {
	targetTime := targetTestTime()
	targetMinute := statsMinute(targetTime)
	validStatsKey := statsKey(targetMinute, "openai", "gpt-5")

	validLegacy := rawLegacyCounters{
		Requests:            2,
		InputTokens:         100,
		OutputTokens:        40,
		CacheReadTokens:     20,
		CacheCreationTokens: 10,
		UnpricedRequests:    0,
		CostUSD:             0.1,
	}

	cases := []struct {
		name        string
		setup       func(tx *bolt.Tx) (key []byte, originalBytes []byte)
		errContains string
	}{
		{
			name: "illegal_legacy_length",
			setup: func(tx *bolt.Tx) ([]byte, []byte) {
				key := validStatsKey
				badVal := make([]byte, legacyCountersSize-1) // 119 bytes
				_ = tx.Bucket(bucketStats).Put(key, badVal)
				return key, badVal
			},
			errContains: "corrupt stats record during migration",
		},
		{
			name: "malformed_stats_key",
			setup: func(tx *bolt.Tx) ([]byte, []byte) {
				badKey := []byte("short") // length < 9
				val := encodeLegacyCountersBytes(validLegacy)
				_ = tx.Bucket(bucketStats).Put(badKey, val)
				return badKey, val
			},
			errContains: "malformed legacy stats key",
		},
		{
			name: "unpriced_exceeds_requests",
			setup: func(tx *bolt.Tx) ([]byte, []byte) {
				badLegacy := validLegacy
				badLegacy.Requests = 2
				badLegacy.UnpricedRequests = 3 // 3 > 2
				val := encodeLegacyCountersBytes(badLegacy)
				_ = tx.Bucket(bucketStats).Put(validStatsKey, val)
				return validStatsKey, val
			},
			errContains: "corrupt stats record during migration",
		},
		{
			name: "complete_token_mismatch",
			setup: func(tx *bolt.Tx) ([]byte, []byte) {
				// Partial stats to trigger recoverRows
				leg := validLegacy
				leg.Requests = 2
				leg.UnpricedRequests = 1
				val := encodeLegacyCountersBytes(leg)
				_ = tx.Bucket(bucketStats).Put(validStatsKey, val)

				// Complete request with invalid token math:
				// UncachedInputTokens = 90, but Input(100) - Read(20) - Write(10) = 70 != 90
				req := makeRetainedRequest(1, "r-bad", targetTime, "openai", "gpt-5", "complete", 90, 20, 10, 40, 0)
				req.InputTokens = 100
				data, _ := json.Marshal(req)
				_ = tx.Bucket(bucketRequests).Put(encodeRequestKey(req.Time, req.Sequence), data)
				return validStatsKey, val
			},
			errContains: "invalid complete token buckets during migration",
		},
		{
			name: "request_key_mismatch",
			setup: func(tx *bolt.Tx) ([]byte, []byte) {
				leg := validLegacy
				leg.Requests = 2
				leg.UnpricedRequests = 1
				val := encodeLegacyCountersBytes(leg)
				_ = tx.Bucket(bucketStats).Put(validStatsKey, val)

				// Key encoded with seq=1, but request JSON payload has seq=2
				req := makeRetainedRequest(2, "r-seq2", targetTime, "openai", "gpt-5", "complete", 70, 20, 10, 40, 0)
				data, _ := json.Marshal(req)
				mismatchedKey := encodeRequestKey(targetTime, 1)
				_ = tx.Bucket(bucketRequests).Put(mismatchedKey, data)
				return validStatsKey, val
			},
			errContains: "retained request key mismatch during migration",
		},
		{
			name: "retained_matched_exceeds_requests",
			setup: func(tx *bolt.Tx) ([]byte, []byte) {
				leg := validLegacy
				leg.Requests = 1
				leg.UnpricedRequests = 1
				val := encodeLegacyCountersBytes(leg)
				_ = tx.Bucket(bucketStats).Put(validStatsKey, val)

				// 2 retained requests matching stats with Requests=1
				req1 := makeRetainedRequest(1, "r1", targetTime, "openai", "gpt-5", "complete", 70, 20, 10, 40, 0)
				d1, _ := json.Marshal(req1)
				_ = tx.Bucket(bucketRequests).Put(encodeRequestKey(req1.Time, req1.Sequence), d1)

				req2 := makeRetainedRequest(2, "r2", targetTime.Add(time.Second), "openai", "gpt-5", "complete", 70, 20, 10, 40, 0)
				d2, _ := json.Marshal(req2)
				_ = tx.Bucket(bucketRequests).Put(encodeRequestKey(req2.Time, req2.Sequence), d2)
				return validStatsKey, val
			},
			errContains: "retained request count exceeds legacy stats",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, nil)
			var checkKey, checkBytes []byte

			setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
				checkKey, checkBytes = tc.setup(tx)
				return nil
			})

			// Attempt Open, expecting failure
			s, err := Open(cfg)
			if err == nil {
				_ = s.Close()
				t.Fatalf("expected Open error containing %q, got nil", tc.errContains)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
			}

			// Fail-safe assertion 1: bbolt lock must be released so bolt.Open succeeds immediately
			db, openErr := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
			if openErr != nil {
				t.Fatalf("expected bbolt lock to be released after failed Open, but got: %v", openErr)
			}
			defer db.Close()

			// Fail-safe assertion 2 & 3: transaction rolled back (marker == 0, stats bytes unchanged)
			err = db.View(func(tx *bolt.Tx) error {
				sb := tx.Bucket(bucketStats)
				if sb == nil {
					return fmt.Errorf("stats bucket missing")
				}
				if seq := sb.Sequence(); seq != 0 {
					t.Errorf("expected Sequence marker to remain 0 after rollback, got %d", seq)
				}
				gotBytes := sb.Get(checkKey)
				if !bytes.Equal(gotBytes, checkBytes) {
					t.Errorf("stats bytes changed despite rollback: got %x, want %x", gotBytes, checkBytes)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("db.View failed: %v", err)
			}
		})
	}
}

// TestMigration_SaturatedOrInvalidSubtraction tests requirement 6:
// Saturated or invalid subtraction in full legacy (Requests == MaxUint64 or InputTokens < CacheRead + CacheCreation)
// must enter the recoverRows branch rather than computing direct subtraction.
func TestMigration_SaturatedOrInvalidSubtraction(t *testing.T) {
	targetTime := targetTestTime()
	targetMinute := statsMinute(targetTime)

	t.Run("input_less_than_cache_read_plus_write", func(t *testing.T) {
		// Legacy record has UnpricedRequests=0, but InputTokens(100) < Read(60) + Write(50).
		// Direct subtraction would underflow uint64 to 18446744073709551606.
		legacy := rawLegacyCounters{
			Requests:            2,
			InputTokens:         100,
			CacheReadTokens:     60,
			CacheCreationTokens: 50, // 60 + 50 = 110 > 100
			OutputTokens:        40,
			UnpricedRequests:    0,
			CostUSD:             0.5,
		}

		sk := statsKey(targetMinute, "openai", "gpt-5")
		cfg := testConfig(t, nil)

		setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
			if err := tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy)); err != nil {
				return err
			}
			// One retained complete request in bucketRequests
			req := makeRetainedRequest(1, "r1", targetTime, "openai", "gpt-5", "complete", 25, 10, 5, 20, 0)
			data, err := json.Marshal(req)
			if err != nil {
				return err
			}
			return tx.Bucket(bucketRequests).Put(encodeRequestKey(req.Time, req.Sequence), data)
		})

		s, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer s.Close()

		err = s.db.View(func(tx *bolt.Tx) error {
			val := tx.Bucket(bucketStats).Get(sk)
			c, ok := decodeCounters(val)
			if !ok {
				t.Fatalf("decodeCounters failed")
			}
			// Recover branch was taken: completeRequests is 1 (from retained request), not 2
			if c.completeRequests != 1 {
				t.Errorf("completeRequests want 1, got %d", c.completeRequests)
			}
			// billableInput is 25 (from retained request), not an underflowed uint64
			if c.billableInput != 25 {
				t.Errorf("billableInput want 25, got %d", c.billableInput)
			}
			if c.billableRead != 10 {
				t.Errorf("billableRead want 10, got %d", c.billableRead)
			}
			if c.billableWrite != 5 {
				t.Errorf("billableWrite want 5, got %d", c.billableWrite)
			}
			if c.billableOutput != 20 {
				t.Errorf("billableOutput want 20, got %d", c.billableOutput)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("db.View failed: %v", err)
		}
	})

	t.Run("requests_max_uint64", func(t *testing.T) {
		// Legacy record has Requests = MaxUint64, UnpricedRequests = 0.
		legacy := rawLegacyCounters{
			Requests:            math.MaxUint64,
			InputTokens:         100,
			CacheReadTokens:     20,
			CacheCreationTokens: 10,
			OutputTokens:        40,
			UnpricedRequests:    0,
			CostUSD:             0.5,
		}

		sk := statsKey(targetMinute, "openai", "gpt-5")
		cfg := testConfig(t, nil)

		setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
			return tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy))
		})

		s, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer s.Close()

		err = s.db.View(func(tx *bolt.Tx) error {
			val := tx.Bucket(bucketStats).Get(sk)
			c, ok := decodeCounters(val)
			if !ok {
				t.Fatalf("decodeCounters failed")
			}
			// Original Requests counter is preserved
			if c.Requests != math.MaxUint64 {
				t.Errorf("Requests want MaxUint64, got %d", c.Requests)
			}
			// Must NOT blindly set completeRequests = MaxUint64; must be 0 because no retained requests
			if c.completeRequests != 0 {
				t.Errorf("completeRequests want 0, got %d", c.completeRequests)
			}
			if c.billableInput != 0 {
				t.Errorf("billableInput want 0, got %d", c.billableInput)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("db.View failed: %v", err)
		}
	})
}

// TestMigration_ReopenAndReprice tests requirement 7:
// Subsequent Open calls do not rescan or remigrate (marker=1, counters stable in DB),
// and changing prices re-evaluates query summary costs correctly.
func TestMigration_ReopenAndReprice(t *testing.T) {
	targetTime := targetTestTime()
	targetMinute := statsMinute(targetTime)

	legacy := rawLegacyCounters{
		Requests:            2,
		InputTokens:         100,
		OutputTokens:        40,
		CacheReadTokens:     30,
		CacheCreationTokens: 20,
		UnpricedRequests:    0,
		CostUSD:             0.5,
	}

	sk := statsKey(targetMinute, "openai", "gpt-5")
	cfg1 := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{
			"gpt-5": {
				Input:         1.0,
				Output:        2.0,
				CacheRead:     0.5,
				CacheCreation: 0.5,
			},
		}
	})

	setupLegacyDB(t, cfg1.DataPath, func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy))
	})

	// First Open
	s1, err := Open(cfg1)
	if err != nil {
		t.Fatalf("First Open failed: %v", err)
	}

	// Calculate cost with price 1:
	// 50*1.0 + 30*0.5 + 20*0.5 + 40*2.0 = 50 + 15 + 10 + 80 = 155 / 1e6 = 0.000155
	sum1, err := s1.Summary(Query{From: targetTime.Add(-time.Minute), To: targetTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("s1.Summary failed: %v", err)
	}
	if !almostEqual(sum1.Totals.CostUSD, 0.000155) {
		t.Errorf("sum1 CostUSD want 0.000155, got %v", sum1.Totals.CostUSD)
	}

	var firstBytes []byte
	_ = s1.db.View(func(tx *bolt.Tx) error {
		firstBytes = append([]byte(nil), tx.Bucket(bucketStats).Get(sk)...)
		return nil
	})

	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close failed: %v", err)
	}

	// Second Open with changed prices
	cfg2 := testConfig(t, func(c *Config) {
		c.DataPath = cfg1.DataPath
		c.Prices = map[string]Price{
			"gpt-5": {
				Input:         2.0,
				Output:        4.0,
				CacheRead:     1.0,
				CacheCreation: 1.0,
			},
		}
	})

	s2, err := Open(cfg2)
	if err != nil {
		t.Fatalf("Second Open failed: %v", err)
	}
	defer s2.Close()

	// Assert sequence marker is still 1 and raw DB bytes did not change
	err = s2.db.View(func(tx *bolt.Tx) error {
		sb := tx.Bucket(bucketStats)
		if seq := sb.Sequence(); seq != 1 {
			t.Errorf("Sequence marker want 1, got %d", seq)
		}
		secondBytes := sb.Get(sk)
		if !bytes.Equal(firstBytes, secondBytes) {
			t.Errorf("DB bytes changed on reopen: got %x, want %x", secondBytes, firstBytes)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("s2.db.View failed: %v", err)
	}

	// Summary re-prices with price 2:
	// 50*2.0 + 30*1.0 + 20*1.0 + 40*4.0 = 100 + 30 + 20 + 160 = 310 / 1e6 = 0.000310
	sum2, err := s2.Summary(Query{From: targetTime.Add(-time.Minute), To: targetTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("s2.Summary failed: %v", err)
	}
	if !almostEqual(sum2.Totals.CostUSD, 0.000310) {
		t.Errorf("sum2 CostUSD want 0.000310, got %v", sum2.Totals.CostUSD)
	}
	if sum2.Totals.Requests != 2 {
		t.Errorf("sum2 Requests want 2, got %d", sum2.Totals.Requests)
	}
}

// TestMigration_RetentionCleanupAndCompaction tests requirement 8:
// Migrated counters persist across retention cleanup of requests and auto-compaction.
func TestMigration_RetentionCleanupAndCompaction(t *testing.T) {
	targetTime := targetTestTime()
	targetMinute := statsMinute(targetTime)
	clk := &clock{t: targetTime}

	cfg := compactConfig(t)
	cfg.StatsRetentionDays = 365
	cfg.RequestRetention = time.Minute
	cfg.Prices = map[string]Price{
		"gpt-5": {Input: 10.0, Output: 20.0},
	}

	sk := statsKey(targetMinute, "openai", "gpt-5")
	legacy := rawLegacyCounters{
		Requests:            2,
		InputTokens:         100,
		OutputTokens:        40,
		CacheReadTokens:     30,
		CacheCreationTokens: 20,
		UnpricedRequests:    0,
		CostUSD:             0.5,
	}

	setupLegacyDB(t, cfg.DataPath, func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketStats).Put(sk, encodeLegacyCountersBytes(legacy)); err != nil {
			return err
		}
		req := makeRetainedRequest(1, "r-clean", targetTime, "openai", "gpt-5", "complete", 50, 30, 20, 40, 0)
		data, err := json.Marshal(req)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketRequests).Put(encodeRequestKey(req.Time, req.Sequence), data)
	})

	s, err := openStore(cfg, time.Hour)
	if err != nil {
		t.Fatalf("openStore failed: %v", err)
	}
	defer s.Close()
	s.setNow(clk.now)

	// Advance clock past request retention (1 min) but within stats retention (365 days)
	clk.advance(10 * time.Minute)

	if err := s.cleanupTx(clk.now()); err != nil {
		t.Fatalf("cleanupTx failed: %v", err)
	}

	// Verify request was expired and deleted
	reqs := allRequests(t, s)
	if len(reqs) != 0 {
		t.Fatalf("expected retained request to be purged, got %d", len(reqs))
	}

	// Trigger compaction
	s.maybeCompact(clk.now())

	// Verify migrated counters are intact after cleanup and compaction
	sumAfter, err := s.Summary(Query{From: targetTime.Add(-time.Minute), To: targetTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary after cleanup/compaction failed: %v", err)
	}
	if sumAfter.Totals.Requests != 2 {
		t.Errorf("Summary Requests want 2, got %d", sumAfter.Totals.Requests)
	}
	if sumAfter.Totals.InputTokens != 100 {
		t.Errorf("Summary InputTokens want 100, got %d", sumAfter.Totals.InputTokens)
	}
	// billableInput=50, billableOutput=40 -> 50*10 + 40*20 = 1300 / 1e6 = 0.0013
	wantCost := 0.0013
	if !almostEqual(sumAfter.Totals.CostUSD, wantCost) {
		t.Errorf("Summary CostUSD want %v, got %v", wantCost, sumAfter.Totals.CostUSD)
	}

	// Raw check
	err = s.db.View(func(tx *bolt.Tx) error {
		sb := tx.Bucket(bucketStats)
		if sb.Sequence() != 1 {
			t.Errorf("Sequence want 1, got %d", sb.Sequence())
		}
		val := sb.Get(sk)
		if len(val) != countersSize {
			t.Fatalf("countersSize want %d, got %d", countersSize, len(val))
		}
		c, ok := decodeCounters(val)
		if !ok {
			t.Fatalf("decodeCounters failed")
		}
		if c.completeRequests != 2 || c.billableInput != 50 {
			t.Errorf("migrated counters corrupted after cleanup/compaction: %+v", c)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("db.View failed: %v", err)
	}
}
