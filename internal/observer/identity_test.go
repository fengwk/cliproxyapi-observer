package observer

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Identities stay stable across reopen and never enqueue or persist key material.
func TestClientIdentityPersistenceAndPrivacy(t *testing.T) {
	cfg := testConfig(t, nil)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().Add(-time.Minute)
	record := usageRecord("identity-first", "openai", "model", at, simpleUsage(10, 5))
	record.APIKey = "fake-private-client-key"
	record.AuthIndex = "0123456789abcdef"
	record.AuthID = "fake-secret-auth-id"
	record.Source = "fake-secret-source"
	if !s.SubmitUsage(record) {
		t.Fatal("enqueue")
	}
	flushAll(t, s)
	first := s.clientKeyID(record.APIKey)
	if !validHexID(first, 64) || first == s.clientKeyID("other-fake-key") {
		t.Fatal("invalid or colliding identity")
	}
	page, err := s.Requests(Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ClientKeyID != first || page.Items[0].AuthIndex != record.AuthIndex {
		t.Fatalf("request identity not retained: err %v", err)
	}
	raw, _ := json.Marshal(page)
	for _, secret := range []string{record.APIKey, record.AuthID, record.Source} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("API leaked private field")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dbRaw, err := os.ReadFile(cfg.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{record.APIKey, record.AuthID, record.Source} {
		if bytes.Contains(dbRaw, []byte(secret)) {
			t.Fatal("database leaked private field")
		}
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.clientKeyID(record.APIKey) != first {
		t.Fatal("identity changed after reopen")
	}
	independent := openTestStore(t, nil)
	if independent.clientKeyID(record.APIKey) == first {
		t.Fatal("independent databases share identities")
	}
	if reopened.clientKeyID("") != "" {
		t.Fatal("absent identity must stay unknown")
	}
}

// Filters select before offset/limit and keep legacy rows explicitly unknown.
func TestRequestIdentityFilteringAndPaging(t *testing.T) {
	s := openTestStore(t, nil)
	at := time.Now().Add(-time.Minute)
	for i := 0; i < 8; i++ {
		r := usageRecord(string(rune('a'+i)), "openai", "model", at.Add(time.Duration(i)*time.Second), simpleUsage(10, 1))
		if i%2 == 0 {
			r.APIKey = "fake-a"
			r.AuthIndex = "0123456789abcdef"
		} else if i != 7 {
			r.APIKey = "fake-b"
			r.AuthIndex = "fedcba9876543210"
		} else {
			r.AuthIndex = "unsafe-value"
		}
		if !s.SubmitUsage(r) {
			t.Fatal("enqueue")
		}
	}
	flushAll(t, s)
	cases := []struct {
		q    Query
		want string
		more bool
	}{
		{Query{ClientKeyID: s.clientKeyID("fake-a"), Limit: 2}, "ge", true},
		{Query{ClientKeyID: s.clientKeyID("fake-a"), Limit: 2, Offset: 2}, "ca", false},
		{Query{AuthIndex: "fedcba9876543210"}, "fdb", false},
		{Query{ClientKeyID: "unknown", AuthIndex: "unknown"}, "h", false},
		{Query{ClientKeyID: s.clientKeyID("fake-a"), AuthIndex: "fedcba9876543210"}, "", false},
		{Query{ClientKeyID: strings.Repeat("0", 64)}, "", false},
	}
	for _, tc := range cases {
		page, err := s.Requests(tc.q)
		if err != nil {
			t.Fatal(err)
		}
		var ids string
		for _, r := range page.Items {
			ids += r.RequestID
		}
		if ids != tc.want || page.HasMore != tc.more {
			t.Fatalf("filter page: got %q more %v; want %q more %v", ids, page.HasMore, tc.want, tc.more)
		}
	}
	if _, err := s.Requests(Query{ClientKeyID: "short"}); err != ErrInvalidQuery {
		t.Fatal("accepted truncated fingerprint")
	}
	if summary, err := s.Summary(Query{AuthIndex: "unknown"}); err != nil || summary.Totals.Requests != 1 {
		t.Fatal("summary did not apply identity filter")
	}
}

// A corrupt secret fails closed instead of silently splitting historical identities.
func TestCorruptClientIdentitySecret(t *testing.T) {
	cfg := testConfig(t, nil)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketIdentity).Put(clientSecretKey, []byte("corrupt"))
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if reopened, err := Open(cfg); err == nil {
		reopened.Close()
		t.Fatal("corrupt secret accepted")
	}
}

// All identity views derive from the same counters, independent of 24h logs.
func TestKeySummaryFiltersGroupsAndLongRetention(t *testing.T) {
	s := openTestStore(t, func(cfg *Config) {
		cfg.Prices = map[string]Price{"m": {Input: 1, Output: 2}}
	})
	now := time.Now().UTC().Truncate(time.Minute)
	s.setNow(func() time.Time { return now })
	authA, authB := "0123456789abcdef", "fedcba9876543210"
	for i, days := range []int{0, 6, 29} {
		r := usageRecord(string(rune('a'+i)), "openai", "m", now.Add(-time.Duration(days)*24*time.Hour-time.Minute), simpleUsage(10, 5))
		r.APIKey = "fake-a"
		r.AuthIndex = authA
		s.SubmitUsage(r)
	}
	r := usageRecord("b-key", "openai", "m", now.Add(-time.Hour), simpleUsage(20, 10))
	r.APIKey, r.AuthIndex, r.Failed = "fake-b", authB, true
	s.SubmitUsage(r)
	s.SubmitUsage(usageRecord("unknown", "openai", "m", now.Add(-2*time.Hour), simpleUsage(5, 2)))
	flushAll(t, s)
	if len(allRequests(t, s)) != 3 {
		t.Fatal("long-retained request rows should have expired")
	}
	for _, tc := range []struct {
		days                       int
		key, auth, provider, model string
		requests                   uint64
	}{
		{30, "", "", "", "", 5},
		{7, s.clientKeyID("fake-a"), "", "", "", 2},
		{30, "", authA, "", "", 3},
		{30, s.clientKeyID("fake-a"), authB, "", "", 0},
		{30, "unknown", "unknown", "", "", 1},
		{30, "", "", "other", "", 0},
		{30, "", "", "", "other", 0},
	} {
		q := Query{From: now.Add(-time.Duration(tc.days) * 24 * time.Hour), To: now.Add(time.Minute),
			ClientKeyID: tc.key, AuthIndex: tc.auth, Provider: tc.provider, Model: tc.model}
		sum, err := s.Summary(q)
		if err != nil || sum.Totals.Requests != tc.requests {
			t.Fatalf("summary filter: %v got %d err %v", tc, sum.Totals.Requests, err)
		}
		// Compare token/cost/latency partitions as well as request totals.
		for _, groups := range [][]KeyGroup{sum.ClientKeys, sum.Credentials} {
			var counters Counters
			for _, g := range groups {
				addCounters(&counters, g.Counters)
			}
			if !equalSummaryCounters(counters, sum.Totals) {
				t.Fatal("key breakdown disagrees with overview")
			}
		}
		var series Counters
		for _, point := range sum.Series {
			addCounters(&series, point.Counters)
		}
		if !equalSummaryCounters(series, sum.Totals) {
			t.Fatal("trend disagrees with overview")
		}
	}
	if _, err := s.Summary(Query{AuthIndex: "invalid"}); err != ErrInvalidQuery {
		t.Fatal("invalid auth filter accepted")
	}
	s.setNow(func() time.Time { return now.Add(366 * 24 * time.Hour) })
	if err := s.runCleanup(); err != nil {
		t.Fatal(err)
	}
	if bucketCount(t, s, bucketStats) != 0 || bucketCount(t, s, bucketKeyStats) != 0 {
		t.Fatal("identity stats ignored configured retention")
	}
}

// Physical swaps must preserve both the HMAC seed and the migration sequence.
func TestIdentityStatsSurviveCompaction(t *testing.T) {
	cfg := compactConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clk := &clock{t: time.Now()}
	fingerprint := s.clientKeyID("fake-key")
	r := usageRecord("identity-old", "openai", "m", clk.now(), simpleUsage(10, 5))
	r.APIKey, r.AuthIndex = "fake-key", "0123456789abcdef"
	s.SubmitUsage(r)
	inflateStore(t, s, clk)
	clk.advance(2 * time.Minute)
	flushAll(t, s)
	if s.Status().Compactions != 1 {
		t.Fatal("physical swap did not happen")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.clientKeyID("fake-key") != fingerprint {
		t.Fatal("compaction lost HMAC seed")
	}
	sum, err := reopened.Summary(Query{ClientKeyID: fingerprint})
	if err != nil || sum.Totals.Requests != 1 {
		t.Fatal("compaction lost per-key totals")
	}
	if err := reopened.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketKeyStats).Sequence() != 1 {
			t.Fatal("compaction lost migration marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func equalSummaryCounters(a, b Counters) bool {
	sameCost := math.Abs(a.CostUSD-b.CostUSD) < 1e-12
	a.CostUSD, b.CostUSD = 0, 0
	return sameCost && reflect.DeepEqual(a, b)
}
