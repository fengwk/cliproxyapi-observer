package observer

import (
	"errors"
	"fmt"
	"testing"
	"time"
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

func TestRequestsCursorPaging(t *testing.T) {
	s := openTestStore(t, nil)
	const total = 250
	seedRequests(t, s, total, func(int) string { return "openai" })

	query := Query{Limit: 50}
	seen := make(map[string]bool, total)
	var lastSeq uint64
	pages := 0
	for {
		page, err := s.Requests(query)
		if err != nil {
			t.Fatalf("Requests: %v", err)
		}
		pages++
		if len(page.Items) == 0 {
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
		if page.NextCursor == "" {
			t.Fatalf("HasMore set but NextCursor empty")
		}
		query.Cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("saw %d records, want %d", len(seen), total)
	}
	if pages != (total+49)/50 {
		t.Fatalf("pages = %d, want %d", pages, (total+49)/50)
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

func TestRequestsRejectsMalformedCursor(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 3, func(int) string { return "openai" })
	for _, cursor := range []string{"not-base64!!", "AAAA", "eyJhIjoxfQ"} {
		if _, err := s.Requests(Query{Cursor: cursor}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidCursor", cursor, err)
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

func TestRequestsDecodesOnlyPageLimit(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 500, func(int) string { return "openai" })
	page, err := s.Requests(Query{Limit: 10})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 10 {
		t.Fatalf("items = %d, want 10", len(page.Items))
	}
	if !page.HasMore {
		t.Fatalf("HasMore should be true with 500 records and limit 10")
	}
}
