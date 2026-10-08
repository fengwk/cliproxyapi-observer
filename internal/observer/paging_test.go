package observer

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func seedRequests(t *testing.T, s *Store, count int, providerFor func(i int) string) time.Time {
	t.Helper()
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	for i := 0; i < count; i++ {
		at := base.Add(time.Duration(i) * time.Millisecond)
		if !s.SubmitUsage(usageRecord(fmt.Sprintf("req-%04d", i), providerFor(i), "gpt-5", at, simpleUsage(10, 5))) {
			t.Fatalf("submit %d rejected", i)
		}
	}
	flushAll(t, s)
	return base
}

// TestRequestsOffsetPaging verifies complete traversal of all records using offset/limit paging,
// ensuring strict descending sequence ordering, no duplicates, accurate page metadata, and proper termination.
func TestRequestsOffsetPaging(t *testing.T) {
	s := openTestStore(t, nil)
	const total = 250
	seedRequests(t, s, total, func(int) string { return "openai" })

	const pageSize = 50
	query := Query{Limit: pageSize, Offset: 0}
	seen := make(map[string]bool, total)
	var lastSeq uint64
	pages := 0
	for {
		page, err := s.Requests(query)
		if err != nil {
			t.Fatalf("Requests(offset=%d): %v", query.Offset, err)
		}
		if page.Offset != query.Offset {
			t.Errorf("page.Offset = %d, want %d", page.Offset, query.Offset)
		}
		if page.Limit != pageSize {
			t.Errorf("page.Limit = %d, want %d", page.Limit, pageSize)
		}
		pages++
		if len(page.Items) == 0 {
			if page.HasMore {
				t.Fatalf("HasMore is true on empty page at offset %d", query.Offset)
			}
			break
		}
		for _, item := range page.Items {
			if seen[item.RequestID] {
				t.Fatalf("duplicate item across pages: %s", item.RequestID)
			}
			seen[item.RequestID] = true
			if lastSeq != 0 && item.Sequence >= lastSeq {
				t.Fatalf("sequence not strictly descending: %d then %d", lastSeq, item.Sequence)
			}
			lastSeq = item.Sequence
		}
		if !page.HasMore {
			break
		}
		query.Offset += len(page.Items)
	}
	if len(seen) != total {
		t.Fatalf("saw %d records, want %d", len(seen), total)
	}
	if pages != total/pageSize {
		t.Fatalf("pages = %d, want %d", pages, total/pageSize)
	}

	// Beyond total records returns an empty page with HasMore=false.
	emptyPage, err := s.Requests(Query{Limit: pageSize, Offset: total})
	if err != nil {
		t.Fatalf("Requests at total: %v", err)
	}
	if len(emptyPage.Items) != 0 || emptyPage.HasMore || emptyPage.Offset != total {
		t.Errorf("expected empty page at offset %d: %+v", total, emptyPage)
	}
}

func TestRequestsFiltersNarrowTheScan(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 40, func(i int) string {
		if i%2 == 0 {
			return "openai"
		}
		return "anthropic"
	})

	page, err := s.Requests(Query{Provider: "anthropic", Limit: 100})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 20 {
		t.Fatalf("provider filter returned %d, want 20", len(page.Items))
	}
	for _, item := range page.Items {
		if item.Provider != "anthropic" {
			t.Fatalf("unexpected provider %q", item.Provider)
		}
	}

	page, err = s.Requests(Query{Model: "does-not-exist"})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("model filter returned %d, want 0", len(page.Items))
	}
}

// TestRequestsRejectsInvalidOffset verifies that negative offsets are rejected with ErrInvalidQuery.
func TestRequestsRejectsInvalidOffset(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 3, func(int) string { return "openai" })
	for _, offset := range []int{-1, -50, math.MinInt32} {
		if _, err := s.Requests(Query{Offset: offset}); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("offset %d: err = %v, want ErrInvalidQuery", offset, err)
		}
	}
}

func TestRequestsRejectsInvertedRange(t *testing.T) {
	s := openTestStore(t, nil)
	now := time.Now()
	if _, err := s.Requests(Query{From: now, To: now.Add(-time.Hour)}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("err = %v, want ErrInvalidQuery", err)
	}
}

// TestRequestsSkipsRawKeysForOffset proves the unfiltered offset prefix skips
// raw bucket keys without decoding: corrupt records inside the skipped prefix
// still occupy their position, so the page boundary does not shift.
func TestRequestsSkipsRawKeysForOffset(t *testing.T) {
	s := openTestStore(t, nil)
	const total = 30
	seedRequests(t, s, total, func(int) string { return "openai" })

	// Corrupt the five newest records (req-0029 .. req-0025). A decoder that
	// silently dropped corrupt rows while counting the offset would place the
	// page boundary five records too far back.
	err := s.db.Update(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRequests)
		var keys [][]byte
		if err := rb.ForEach(func(k, _ []byte) error {
			keys = append(keys, append([]byte(nil), k...))
			return nil
		}); err != nil {
			return err
		}
		for _, k := range keys[len(keys)-5:] {
			if err := rb.Put(k, []byte("not-json")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("corrupt prefix: %v", err)
	}

	page, err := s.Requests(Query{Offset: 10, Limit: 5})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("items = %d, want 5", len(page.Items))
	}
	want := []string{"req-0019", "req-0018", "req-0017", "req-0016", "req-0015"}
	for i, item := range page.Items {
		if item.RequestID != want[i] {
			t.Fatalf("item[%d] = %s, want %s (raw keys not skipped positionally)", i, item.RequestID, want[i])
		}
	}
}

// TestRequestsOffsetWithFilter verifies that offset pagination with provider/model filters
// skips only records matching the filter, rather than skipping raw unfiltered records.
func TestRequestsOffsetWithFilter(t *testing.T) {
	s := openTestStore(t, nil)
	// Seed 40 total requests: 20 "openai" and 20 "anthropic" interleaved.
	seedRequests(t, s, 40, func(i int) string {
		if i%2 == 0 {
			return "openai"
		}
		return "anthropic"
	})

	// Query anthropic with offset=10, limit=5 (total 20 anthropic requests exist).
	page, err := s.Requests(Query{Provider: "anthropic", Offset: 10, Limit: 5})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("got %d items, want 5", len(page.Items))
	}
	if !page.HasMore {
		t.Fatalf("expected HasMore=true for offset 10 of 20 filtered items with limit 5")
	}
	for _, it := range page.Items {
		if it.Provider != "anthropic" {
			t.Errorf("unexpected provider %q in filtered page", it.Provider)
		}
	}

	// Query last page of anthropic (offset 15, limit 5) -> 5 items, HasMore=false.
	pageLast, err := s.Requests(Query{Provider: "anthropic", Offset: 15, Limit: 5})
	if err != nil {
		t.Fatalf("Requests last page: %v", err)
	}
	if len(pageLast.Items) != 5 || pageLast.HasMore {
		t.Errorf("expected 5 items and HasMore=false, got len=%d, HasMore=%v", len(pageLast.Items), pageLast.HasMore)
	}

	// Query beyond anthropic count (offset 20, limit 5) -> 0 items, HasMore=false.
	pageEmpty, err := s.Requests(Query{Provider: "anthropic", Offset: 20, Limit: 5})
	if err != nil {
		t.Fatalf("Requests beyond end: %v", err)
	}
	if len(pageEmpty.Items) != 0 || pageEmpty.HasMore {
		t.Errorf("expected empty page and HasMore=false beyond end, got %+v", pageEmpty)
	}
}

// TestRequestsTiesOrderingAndPaging verifies that records sharing identical timestamps
// maintain stable newest-first ordering by sequence across offset page boundaries.
func TestRequestsTiesOrderingAndPaging(t *testing.T) {
	s := openTestStore(t, nil)
	baseTime := time.Now().Add(-time.Hour).Truncate(time.Second)

	// Submit 10 records sharing the EXACT same timestamp.
	const tiesCount = 10
	for i := 0; i < tiesCount; i++ {
		if !s.SubmitUsage(usageRecord(fmt.Sprintf("tie-%02d", i), "openai", "gpt-5", baseTime, simpleUsage(1, 1))) {
			t.Fatalf("submit tie %d failed", i)
		}
	}
	flushAll(t, s)

	// Fetch across pages with limit=3.
	var allItems []Request
	for offset := 0; offset < tiesCount; offset += 3 {
		page, err := s.Requests(Query{Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("Requests(offset=%d): %v", offset, err)
		}
		allItems = append(allItems, page.Items...)
	}

	if len(allItems) != tiesCount {
		t.Fatalf("got %d tie items, want %d", len(allItems), tiesCount)
	}
	// Verify strict descending sequence order even though timestamps are identical.
	for i := 1; i < len(allItems); i++ {
		if allItems[i].Sequence >= allItems[i-1].Sequence {
			t.Errorf("tie sequence not descending: index %d (%d) >= index %d (%d)",
				i, allItems[i].Sequence, i-1, allItems[i-1].Sequence)
		}
	}
}

// TestRequestsPagingRoundTrip verifies that paging forward then backward (offset 0 -> offset 5 -> offset 0)
// retrieves identical data without discrepancies or data drift.
func TestRequestsPagingRoundTrip(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 20, func(int) string { return "openai" })

	page0A, err := s.Requests(Query{Limit: 5, Offset: 0})
	if err != nil {
		t.Fatalf("page 0 first fetch: %v", err)
	}
	page1, err := s.Requests(Query{Limit: 5, Offset: 5})
	if err != nil {
		t.Fatalf("page 1 fetch: %v", err)
	}
	page0B, err := s.Requests(Query{Limit: 5, Offset: 0})
	if err != nil {
		t.Fatalf("page 0 second fetch: %v", err)
	}

	if len(page0A.Items) != 5 || len(page1.Items) != 5 || len(page0B.Items) != 5 {
		t.Fatalf("unexpected page sizes: %d, %d, %d", len(page0A.Items), len(page1.Items), len(page0B.Items))
	}
	for i := 0; i < 5; i++ {
		if page0A.Items[i].RequestID != page0B.Items[i].RequestID {
			t.Errorf("roundtrip page 0 mismatch at %d: %s vs %s",
				i, page0A.Items[i].RequestID, page0B.Items[i].RequestID)
		}
		if page0A.Items[i].RequestID == page1.Items[i].RequestID {
			t.Errorf("overlap between page 0 and page 1 at %d: %s", i, page0A.Items[i].RequestID)
		}
	}
}

// TestRequestsNewInsertsShiftOffset verifies the contract rule that new incoming requests
// prepend to the newest-first stream, shifting subsequent offsets cleanly.
func TestRequestsNewInsertsShiftOffset(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 5, func(int) string { return "openai" })

	p0Before, err := s.Requests(Query{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("Requests before insert: %v", err)
	}
	prevTopID := p0Before.Items[0].RequestID

	// Insert a brand new request with a newer timestamp.
	newerTime := time.Now().UTC()
	if !s.SubmitUsage(usageRecord("brand-new", "openai", "gpt-5", newerTime, simpleUsage(1, 1))) {
		t.Fatalf("submit new request rejected")
	}
	flushAll(t, s)

	// Now offset 0 returns the new request.
	p0After, err := s.Requests(Query{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("Requests after insert: %v", err)
	}
	if p0After.Items[0].RequestID != "brand-new" {
		t.Errorf("new top request = %s, want brand-new", p0After.Items[0].RequestID)
	}

	// Offset 1 now returns what was previously offset 0.
	p1After, err := s.Requests(Query{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("Requests at offset 1: %v", err)
	}
	if p1After.Items[0].RequestID != prevTopID {
		t.Errorf("shifted request at offset 1 = %s, want %s", p1After.Items[0].RequestID, prevTopID)
	}
}

// TestRequestsRetentionShiftsWindow verifies that requests falling outside the retention window
// are excluded from offset paging, and clamping works properly.
func TestRequestsRetentionShiftsWindow(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s := openTestStore(t, func(c *Config) { c.RequestRetention = 2 * time.Hour })
	s.setNow(clk.now)

	// Seed 2 old requests at clk.now.
	atOld := clk.now()
	s.SubmitUsage(usageRecord("old-1", "openai", "gpt-5", atOld, simpleUsage(1, 1)))
	s.SubmitUsage(usageRecord("old-2", "openai", "gpt-5", atOld.Add(time.Millisecond), simpleUsage(1, 1)))
	flushAll(t, s)

	// Advance clock by 90 minutes and add 2 newer requests.
	clk.advance(90 * time.Minute)
	atNew := clk.now()
	s.SubmitUsage(usageRecord("new-1", "openai", "gpt-5", atNew, simpleUsage(1, 1)))
	s.SubmitUsage(usageRecord("new-2", "openai", "gpt-5", atNew.Add(time.Millisecond), simpleUsage(1, 1)))
	flushAll(t, s)

	// Step forward slightly so atNew is strictly before s.now() (the upper query bound).
	clk.advance(time.Second)

	// All 4 are currently retained.
	pAll, err := s.Requests(Query{Limit: 10, Offset: 0})
	if err != nil || len(pAll.Items) != 4 {
		t.Fatalf("expected 4 items before retention expiry, got %d, err: %v", len(pAll.Items), err)
	}

	// Advance clock by another 60 minutes. Now old-1 and old-2 are older than 2 hours.
	clk.advance(60 * time.Minute)

	pRetained, err := s.Requests(Query{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("Requests after retention expiry: %v", err)
	}
	if len(pRetained.Items) != 2 {
		t.Fatalf("expected 2 retained items, got %d", len(pRetained.Items))
	}
	for _, it := range pRetained.Items {
		if it.RequestID == "old-1" || it.RequestID == "old-2" {
			t.Errorf("expired item %s appeared in query", it.RequestID)
		}
	}
}

// TestRequestsCorruptRecordHandling verifies that corrupted JSON records in storage
// are safely skipped both during raw key offset skipping and during item decoding,
// without panicking or returning an error.
func TestRequestsCorruptRecordHandling(t *testing.T) {
	s := openTestStore(t, nil)
	baseTime := time.Now().Add(-time.Hour).Truncate(time.Second)

	// Insert a valid request.
	s.SubmitUsage(usageRecord("valid-1", "openai", "gpt-5", baseTime, simpleUsage(1, 1)))
	flushAll(t, s)

	// Manually inject a corrupt record with a newer timestamp in the requests bucket.
	corruptTime := baseTime.Add(time.Millisecond)
	err := s.db.Update(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRequests)
		return rb.Put(encodeRequestKey(corruptTime, 999), []byte("this is not json!{corrupt}"))
	})
	if err != nil {
		t.Fatalf("inject corrupt record: %v", err)
	}

	// Insert another valid request even newer.
	newerTime := baseTime.Add(2 * time.Millisecond)
	s.SubmitUsage(usageRecord("valid-2", "openai", "gpt-5", newerTime, simpleUsage(1, 1)))
	flushAll(t, s)

	// Query without filter:
	// Order from newest to oldest: valid-2, corrupt, valid-1.
	// offset 0 with limit 10 should return valid-2 and valid-1 (corrupt is skipped during decoding).
	page, err := s.Requests(Query{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("Requests with corrupt record: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 valid items decoded, got %d", len(page.Items))
	}
	if page.Items[0].RequestID != "valid-2" || page.Items[1].RequestID != "valid-1" {
		t.Errorf("unexpected items returned: %+v", page.Items)
	}

	// Query with offset=1 without filter:
	// Raw key for valid-2 is skipped (offset=1). Corrupt record is skipped during decode.
	// Valid-1 is decoded and returned!
	pageOffset1, err := s.Requests(Query{Limit: 10, Offset: 1})
	if err != nil {
		t.Fatalf("Requests offset=1: %v", err)
	}
	if len(pageOffset1.Items) != 1 || pageOffset1.Items[0].RequestID != "valid-1" {
		t.Errorf("expected [valid-1] at offset 1, got %+v", pageOffset1.Items)
	}

	// Query with filter:
	// Corrupt record unmarshal fails and is safely skipped.
	pageFiltered, err := s.Requests(Query{Provider: "openai", Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("filtered Requests: %v", err)
	}
	if len(pageFiltered.Items) != 2 {
		t.Fatalf("expected 2 filtered items, got %d", len(pageFiltered.Items))
	}
}
