package observer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func benchStore(b *testing.B) *Store {
	b.Helper()
	cfg := Config{
		DataPath:            filepath.Join(b.TempDir(), "bench.db"),
		StatsRetentionDays:  365,
		RequestRetention:    24 * time.Hour,
		BodyRetention:       24 * time.Hour,
		MaxBodyBytes:        1 << 20,
		MaxBodyStorageBytes: 8 << 20,
		FlushInterval:       20 * time.Millisecond,
	}
	s, err := Open(cfg)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	return s
}

// BenchmarkRequestsPage proves a bounded page only decodes the requested count
// even when the store holds many more records.
func BenchmarkRequestsPage50(b *testing.B) {
	s := benchStore(b)
	defer s.Close()
	ctx := context.Background()

	const seed = 20000
	base := time.Now().Add(-time.Hour)
	for i := 0; i < seed; i++ {
		at := base.Add(time.Duration(i) * time.Microsecond)
		for !s.SubmitUsage(usageRecord(fmt.Sprintf("bench-%05d", i), "openai", "gpt-5", at, simpleUsage(10, 5))) {
			if err := s.Flush(ctx); err != nil {
				b.Fatalf("Flush: %v", err)
			}
		}
		if i%2000 == 0 {
			if err := s.Flush(ctx); err != nil {
				b.Fatalf("Flush: %v", err)
			}
		}
	}
	if err := s.Flush(ctx); err != nil {
		b.Fatalf("Flush: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := s.Requests(Query{Limit: 50})
		if err != nil {
			b.Fatalf("Requests: %v", err)
		}
		if len(page.Items) != 50 {
			b.Fatalf("page size = %d, want 50", len(page.Items))
		}
	}
}

// BenchmarkSubmitUsage measures the nonblocking ingestion hot path.
func BenchmarkSubmitUsage(b *testing.B) {
	s := benchStore(b)
	defer s.Close()
	at := time.Now().Add(-time.Minute)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SubmitUsage(usageRecord(fmt.Sprintf("bench-%d", i), "openai", "gpt-5", at, simpleUsage(10, 5)))
	}
}
