package observer

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

// subsetCacheUsage builds an OpenAI/subset complete record where the cache
// tokens sit inside the input total: total input = uncached + read + write.
func subsetCacheUsage(inputTotal, read, write, output int64) pluginapi.UsageDetail {
	return pluginapi.UsageDetail{
		InputTokens: inputTotal, CacheReadTokens: read, CacheCreationTokens: write, OutputTokens: output,
	}
}

// TestSummaryCacheAccountingTokenWeighted proves the projection is token
// weighted: 80/1000 = 8%, never the mean of per-request shares (40%) nor the
// fraction of requests with a cache hit (50%).
func TestSummaryCacheAccountingTokenWeighted(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	at := from.Add(10 * time.Second)
	s.SubmitUsage(usageRecord("w1", "openai", "gpt-5", at, subsetCacheUsage(100, 80, 10, 20)))
	s.SubmitUsage(usageRecord("w2", "openai", "gpt-5", at.Add(time.Second), subsetCacheUsage(900, 0, 0, 100)))
	flushAll(t, s)

	sum, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	ca := sum.Totals.CacheAccounting
	if ca == nil {
		t.Fatalf("cache_accounting is null")
	}
	if ca.Requests != 2 || ca.InputTokens != 1000 || ca.CacheReadTokens != 80 {
		t.Fatalf("cache_accounting = %+v, want requests=2 input=1000 read=80", ca)
	}
	if ratio := float64(ca.CacheReadTokens) / float64(ca.InputTokens); !approx(ratio, 0.08) {
		t.Errorf("token-weighted ratio = %v, want 0.08", ratio)
	}
	// The public raw counters keep every record and must not be replaced.
	if sum.Totals.InputTokens != 1000 || sum.Totals.CacheReadTokens != 80 {
		t.Errorf("public raw totals changed: %+v", sum.Totals)
	}
}

// TestSummaryCacheAccountingIgnoresAmbiguousRecords proves unclassified cache
// reads, contradictory "complete" claims and usage-unavailable failures only
// reach the public raw counters, never the complete token partition.
func TestSummaryCacheAccountingIgnoresAmbiguousRecords(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	at := from.Add(10 * time.Second)
	// Two genuinely complete records: input 1000, read 80.
	s.SubmitUsage(usageRecord("ok1", "openai", "gpt-5", at, subsetCacheUsage(100, 80, 10, 20)))
	s.SubmitUsage(usageRecord("ok2", "openai", "gpt-5", at.Add(time.Second), subsetCacheUsage(900, 0, 0, 100)))
	// Unknown-provider record with a large positive cache read: unclassified.
	s.SubmitUsage(usageRecord("unk", "mystery", "gpt-5", at,
		pluginapi.UsageDetail{InputTokens: 1000, CacheReadTokens: 500, OutputTokens: 10}))
	// Contradictory record that claims complete but input < read + write.
	s.SubmitUsage(usageRecord("bad", "openai", "gpt-5", at,
		pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 60, OutputTokens: 10}))
	// Failed request whose usage is unavailable: complete, but zero tokens.
	failed := usageRecord("fail", "openai", "gpt-5", at, pluginapi.UsageDetail{})
	failed.Failed = true
	s.SubmitUsage(failed)
	flushAll(t, s)

	sum, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	ca := sum.Totals.CacheAccounting
	if ca == nil {
		t.Fatalf("cache_accounting is null")
	}
	if ca.InputTokens != 1000 || ca.CacheReadTokens != 80 {
		t.Fatalf("ambiguous records polluted the token partition: %+v", ca)
	}
	// The zero-input complete failure counts as complete coverage; the two
	// complete-usage records plus it make three.
	if ca.Requests != 3 {
		t.Errorf("cache_accounting.Requests = %d, want 3", ca.Requests)
	}
	if sum.Totals.Requests != 5 {
		t.Errorf("public Requests = %d, want 5", sum.Totals.Requests)
	}
	// Public raw counters still include every record.
	if sum.Totals.InputTokens != 2100 || sum.Totals.CacheReadTokens != 630 {
		t.Errorf("public raw totals = input %d read %d, want 2100/630",
			sum.Totals.InputTokens, sum.Totals.CacheReadTokens)
	}
}

// TestSummaryCacheAccountingZeroInputCountsCoverage pins the contract that a
// zero-input complete record counts as complete coverage while contributing
// nothing to the token numerator or denominator.
func TestSummaryCacheAccountingZeroInputCountsCoverage(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	at := from.Add(10 * time.Second)
	s.SubmitUsage(usageRecord("zero", "openai", "gpt-5", at, pluginapi.UsageDetail{OutputTokens: 5}))
	s.SubmitUsage(usageRecord("real", "openai", "gpt-5", at.Add(time.Second), subsetCacheUsage(100, 80, 10, 20)))
	flushAll(t, s)

	sum, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	ca := sum.Totals.CacheAccounting
	if ca == nil || ca.Requests != 2 || ca.InputTokens != 100 || ca.CacheReadTokens != 80 {
		t.Fatalf("cache_accounting = %+v, want requests=2 input=100 read=80", ca)
	}
}

// TestCacheAccountingForInvariants exercises the projection directly for the
// saturation, overflow and coherence guards that must yield null, and for the
// valid empty/normal partitions that must yield an object.
func TestCacheAccountingForInvariants(t *testing.T) {
	cases := []struct {
		name    string
		in      Counters
		wantNil bool
		want    CacheAccounting
	}{
		{
			name: "empty complete partition stays an object",
			in:   Counters{Requests: 5},
			want: CacheAccounting{},
		},
		{
			name: "normal complete partition",
			in: Counters{Requests: 2, InputTokens: 1000, CacheReadTokens: 80, CacheCreationTokens: 10,
				completeRequests: 2, billableInput: 910, billableRead: 80, billableWrite: 10, billableOutput: 120},
			want: CacheAccounting{Requests: 2, InputTokens: 1000, CacheReadTokens: 80},
		},
		{
			name: "zero-input complete record",
			in:   Counters{Requests: 1, completeRequests: 1, billableOutput: 5},
			want: CacheAccounting{Requests: 1},
		},
		{
			name:    "complete exceeds requests",
			in:      Counters{Requests: 1, completeRequests: 2},
			wantNil: true,
		},
		{
			name:    "stray partition without a complete request",
			in:      Counters{Requests: 1, billableInput: 5},
			wantNil: true,
		},
		{
			name:    "stray output partition without a complete request",
			in:      Counters{Requests: 1, billableOutput: 5},
			wantNil: true,
		},
		{
			name:    "completeRequests saturated",
			in:      Counters{Requests: math.MaxUint64, completeRequests: math.MaxUint64, billableInput: 1},
			wantNil: true,
		},
		{
			name:    "billableInput saturated",
			in:      Counters{Requests: 1, InputTokens: math.MaxUint64, completeRequests: 1, billableInput: math.MaxUint64},
			wantNil: true,
		},
		{
			name:    "billableRead saturated",
			in:      Counters{Requests: 1, InputTokens: math.MaxUint64, CacheReadTokens: math.MaxUint64, completeRequests: 1, billableRead: math.MaxUint64},
			wantNil: true,
		},
		{
			name:    "billableWrite saturated",
			in:      Counters{Requests: 1, InputTokens: math.MaxUint64, completeRequests: 1, billableWrite: math.MaxUint64},
			wantNil: true,
		},
		{
			name: "merged complete input overflows to the sentinel",
			in: Counters{Requests: 2, InputTokens: math.MaxUint64, CacheReadTokens: 5,
				completeRequests: 2, billableInput: math.MaxUint64 - 1, billableRead: 5},
			wantNil: true,
		},
		{
			name:    "complete read exceeds the public cache read total",
			in:      Counters{Requests: 1, InputTokens: 10, CacheReadTokens: 1, completeRequests: 1, billableInput: 5, billableRead: 5},
			wantNil: true,
		},
		{
			name:    "complete write exceeds the public cache creation total",
			in:      Counters{Requests: 1, InputTokens: 10, CacheCreationTokens: 1, completeRequests: 1, billableInput: 5, billableWrite: 5},
			wantNil: true,
		},
		{
			name:    "complete input exceeds the public input total",
			in:      Counters{Requests: 1, InputTokens: 5, completeRequests: 1, billableInput: 10},
			wantNil: true,
		},
		{
			name: "saturated public requests does not invalidate a valid partition",
			in: Counters{Requests: math.MaxUint64, InputTokens: 1000, CacheReadTokens: 80, CacheCreationTokens: 10,
				completeRequests: 2, billableInput: 910, billableRead: 80, billableWrite: 10},
			want: CacheAccounting{Requests: 2, InputTokens: 1000, CacheReadTokens: 80},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheAccountingFor(tc.in)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("cacheAccountingFor = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("cacheAccountingFor = nil, want %+v", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("cacheAccountingFor = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// TestSummaryCacheAccountingSaturatedRowIsNull proves a saturated persisted
// aggregate surfaces as a JSON null instead of a truncated ratio basis.
func TestSummaryCacheAccountingSaturatedRowIsNull(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	s.SubmitUsage(usageRecord("r1", "openai", "gpt-5", from.Add(time.Second), subsetCacheUsage(100, 80, 10, 20)))
	flushAll(t, s)

	// Saturate the persisted complete-input partition directly.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketStats)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			counters, ok := decodeCounters(v)
			if !ok {
				return errCorruptFixture
			}
			counters.billableInput = math.MaxUint64
			if err := b.Put(k, encodeCounters(counters)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("saturate stats: %v", err)
	}

	sum, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Totals.CacheAccounting != nil {
		t.Fatalf("saturated aggregate projected %+v, want null", sum.Totals.CacheAccounting)
	}
}

// TestSummaryCacheAccountingConsistentAcrossViews checks that the projection is
// identical in Totals, Groups, the downsampled Series, ClientKeys and
// Credentials, survives close/reopen, and does not depend on the price config.
func TestSummaryCacheAccountingConsistentAcrossViews(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{"model-a": {Input: 2, Output: 3, CacheRead: 1}}
		c.PriceRules = []PriceRule{{Model: "model-a", Price: Price{Input: 5, Output: 5}}}
	})
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.setNow(clk.now)

	from := clk.now()
	const minutes = 320 // exceeds maxSeriesPoints to force downsampling.
	const auth = "0123456789abcdef"
	for m := 0; m < minutes; m++ {
		r := usageRecord(fmt.Sprintf("c-%d", m), "openai", "model-a",
			from.Add(time.Duration(m)*time.Minute).Add(time.Second), subsetCacheUsage(100, 80, 10, 20))
		r.APIKey = "fake-key"
		r.AuthIndex = auth
		s.SubmitUsage(r)
	}
	flushAll(t, s)

	to := from.Add(minutes * time.Minute)
	sum, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	const wantReq, wantInput, wantRead = minutes, minutes * 100, minutes * 80
	assertCache := func(where string, ca *CacheAccounting) {
		t.Helper()
		if ca == nil {
			t.Fatalf("%s: cache_accounting is null", where)
		}
		if ca.Requests != wantReq || ca.InputTokens != wantInput || ca.CacheReadTokens != wantRead {
			t.Fatalf("%s: cache_accounting = %+v, want %d/%d/%d", where, ca, wantReq, wantInput, wantRead)
		}
	}
	assertCache("totals", sum.Totals.CacheAccounting)
	if len(sum.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(sum.Groups))
	}
	assertCache("group", sum.Groups[0].CacheAccounting)
	if len(sum.ClientKeys) != 1 || len(sum.Credentials) != 1 {
		t.Fatalf("key views = %d/%d, want 1/1", len(sum.ClientKeys), len(sum.Credentials))
	}
	assertCache("client key", sum.ClientKeys[0].CacheAccounting)
	assertCache("credential", sum.Credentials[0].CacheAccounting)
	if len(sum.Series) > maxSeriesPoints {
		t.Fatalf("series points = %d, want <= %d", len(sum.Series), maxSeriesPoints)
	}
	var seriesReq, seriesInput, seriesRead uint64
	for _, p := range sum.Series {
		if p.CacheAccounting == nil {
			t.Fatalf("series point has null cache_accounting")
		}
		seriesReq += p.CacheAccounting.Requests
		seriesInput += p.CacheAccounting.InputTokens
		seriesRead += p.CacheAccounting.CacheReadTokens
	}
	if seriesReq != wantReq || seriesInput != wantInput || seriesRead != wantRead {
		t.Fatalf("series sum = %d/%d/%d, want %d/%d/%d",
			seriesReq, seriesInput, seriesRead, wantReq, wantInput, wantRead)
	}

	// Provider/model filters keep the same complete basis for the subset.
	filtered, err := s.Summary(Query{From: from, To: to, Provider: "openai", Model: "model-a"})
	if err != nil {
		t.Fatalf("filtered Summary: %v", err)
	}
	assertCache("filtered totals", filtered.Totals.CacheAccounting)
	keyFiltered, err := s.Summary(Query{From: from, To: to, ClientKeyID: s.clientKeyID("fake-key"), AuthIndex: auth})
	if err != nil {
		t.Fatalf("key-filtered Summary: %v", err)
	}
	assertCache("key-filtered totals", keyFiltered.Totals.CacheAccounting)

	// Close and reopen with an unrelated price config: the projection is
	// derived from the persisted complete partition, not from pricing.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cfg.Prices = map[string]Price{"model-a": {Input: 999}}
	cfg.PriceRules = nil
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopened.setNow(clk.now)
	reopenedSum, err := reopened.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("reopened Summary: %v", err)
	}
	assertCache("reopened totals", reopenedSum.Totals.CacheAccounting)
}

// TestSummaryCacheAccountingEmptyRangeKeepsObject pins that an empty query
// still projects a zeroed object rather than a null.
func TestSummaryCacheAccountingEmptyRangeKeepsObject(t *testing.T) {
	s := openTestStore(t, nil)
	from := time.Now().Add(-time.Hour)
	sum, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Totals.CacheAccounting == nil {
		t.Fatal("empty summary has null cache_accounting")
	}
	if *sum.Totals.CacheAccounting != (CacheAccounting{}) {
		t.Fatalf("empty summary cache_accounting = %+v, want zero object", *sum.Totals.CacheAccounting)
	}
}

// TestCacheAccountingJSONShape verifies the public JSON contract of the
// embedded counters: provider/model/time/id survive embedding, the derived
// field uses its documented keys, nil is null, and the private partitions never
// leak.
func TestCacheAccountingJSONShape(t *testing.T) {
	ca := &CacheAccounting{Requests: 2, InputTokens: 1000, CacheReadTokens: 80}
	group := Group{Provider: "openai", Model: "gpt-5", Counters: Counters{CacheAccounting: ca}}
	point := Point{Time: time.Unix(0, 0).UTC(), Counters: Counters{CacheAccounting: ca}}
	keyGroup := KeyGroup{ID: "id-1", Counters: Counters{CacheAccounting: ca}}

	for name, value := range map[string]any{"group": group, "point": point, "key": keyGroup} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		text := string(raw)
		for _, forbidden := range []string{"completeRequests", "billable", "complete_requests"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s JSON leaked %q: %s", name, forbidden, text)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("%s unmarshal: %v", name, err)
		}
		if _, ok := fields["cache_accounting"]; !ok {
			t.Fatalf("%s JSON missing cache_accounting: %s", name, text)
		}
		var got CacheAccounting
		if err := json.Unmarshal(fields["cache_accounting"], &got); err != nil {
			t.Fatalf("%s cache_accounting: %v", name, err)
		}
		if got != *ca {
			t.Fatalf("%s cache_accounting = %+v, want %+v", name, got, *ca)
		}
	}
	if raw, _ := json.Marshal(group); !strings.Contains(string(raw), `"provider":"openai"`) ||
		!strings.Contains(string(raw), `"model":"gpt-5"`) {
		t.Fatalf("group identity lost: %s", raw)
	}
	if raw, _ := json.Marshal(point); !strings.Contains(string(raw), `"time":`) {
		t.Fatalf("point time lost: %s", raw)
	}
	if raw, _ := json.Marshal(keyGroup); !strings.Contains(string(raw), `"id":"id-1"`) {
		t.Fatalf("key group id lost: %s", raw)
	}

	nilGroup, _ := json.Marshal(Group{Provider: "openai", Model: "gpt-5"})
	if !strings.Contains(string(nilGroup), `"cache_accounting":null`) {
		t.Fatalf("nil projection must serialize as null: %s", nilGroup)
	}
}
