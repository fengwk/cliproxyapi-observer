package observer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestStoreSubmitFlushRead(t *testing.T) {
	s := openTestStore(t, nil)
	now := time.Now().Add(-time.Minute)
	if ok := s.SubmitUsage(usageRecord("req-1", "openai", "gpt-5", now, simpleUsage(100, 50))); !ok {
		t.Fatalf("SubmitUsage rejected")
	}
	flushAll(t, s)

	page, err := s.Requests(Query{})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	item := page.Items[0]
	if item.RequestID != "req-1" || item.TraceID != "trace-req-1" || item.Model != "gpt-5" {
		t.Errorf("record = %+v", item)
	}
	if item.Sequence == 0 {
		t.Errorf("sequence not assigned")
	}
	if item.AccountingQuality != "complete" {
		t.Errorf("quality = %q", item.AccountingQuality)
	}
	if page.HasMore || page.NextCursor != "" {
		t.Errorf("unexpected paging state: %+v", page)
	}
}

func TestSubmitUsageRejectsEmptyRequestID(t *testing.T) {
	s := openTestStore(t, nil)
	if s.SubmitUsage(usageRecord("", "openai", "gpt-5", time.Now(), simpleUsage(1, 1))) {
		t.Fatalf("empty request id accepted")
	}
	flushAll(t, s)
	if got := s.Status().DroppedUsage; got != 1 {
		t.Errorf("DroppedUsage = %d, want 1", got)
	}
}

func TestHistoricalCostSnapshotSurvivesReopen(t *testing.T) {
	cfg := testConfig(t, func(c *Config) {
		c.Prices = map[string]Price{"gpt-5": {Input: 10, Output: 10}}
	})
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	at := time.Now().Add(-time.Minute)
	if !s.SubmitUsage(usageRecord("cost-1", "openai", "gpt-5", at, simpleUsage(1000, 0))) {
		t.Fatalf("submit rejected")
	}
	flushAll(t, s)
	page, _ := s.Requests(Query{})
	if len(page.Items) != 1 || page.Items[0].CostUSD == nil {
		t.Fatalf("no priced request: %+v", page.Items)
	}
	original := *page.Items[0].CostUSD
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen with completely different prices: the stored cost must not move.
	cfg2 := cfg
	cfg2.Prices = map[string]Price{"gpt-5": {Input: 0.0001, Output: 0.0001}}
	s2, err := Open(cfg2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	page2, err := s2.Requests(Query{})
	if err != nil {
		t.Fatalf("Requests after reopen: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].CostUSD == nil {
		t.Fatalf("record lost on reopen: %+v", page2.Items)
	}
	if *page2.Items[0].CostUSD != original {
		t.Errorf("historical cost changed: %v -> %v", original, *page2.Items[0].CostUSD)
	}
}

func TestFlushAndReadsAfterCloseReturnErrClosed(t *testing.T) {
	s := openTestStore(t, nil)
	flushAll(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Flush(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after close = %v, want ErrClosed", err)
	}
	if _, err := s.Requests(Query{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Requests after close = %v, want ErrClosed", err)
	}
	if _, err := s.Summary(Query{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Summary after close = %v, want ErrClosed", err)
	}
	if _, err := s.Body("x"); !errors.Is(err, ErrClosed) {
		t.Errorf("Body after close = %v, want ErrClosed", err)
	}
}

func TestCaptureDisabledDoesNotStoreBodies(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = false })
	at := time.Now().Add(-time.Minute)
	s.SubmitUsage(usageRecord("nb-1", "openai", "gpt-5", at, simpleUsage(1, 1)))
	if s.Capture(pluginapi.RequestInterceptRequest{RequestID: "nb-1", Body: []byte(`{"a":1}`)}) {
		t.Errorf("Capture accepted while disabled")
	}
	flushAll(t, s)
	if got := s.Status().DroppedBodies; got != 0 {
		t.Errorf("DroppedBodies = %d, want 0 when disabled", got)
	}
	if _, err := s.Body("nb-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body = %v, want ErrNotFound", err)
	}
	page, _ := s.Requests(Query{})
	if len(page.Items) != 1 || page.Items[0].BodyAvailable {
		t.Errorf("BodyAvailable should be false when capture disabled: %+v", page.Items)
	}
}

func TestWriterFailureCountsWriteErrors(t *testing.T) {
	s := openTestStore(t, nil)
	at := time.Now().Add(-time.Minute)
	if !s.SubmitUsage(usageRecord("wf-1", "openai", "gpt-5", at, simpleUsage(1, 1))) {
		t.Fatalf("baseline submit rejected")
	}
	flushAll(t, s)

	// Force the underlying database closed so the next write fails.
	if err := s.db.Close(); err != nil {
		t.Fatalf("force close: %v", err)
	}
	s.SubmitUsage(usageRecord("wf-2", "openai", "gpt-5", at, simpleUsage(1, 1)))
	flushAll(t, s)
	if got := s.Status().WriteErrors; got == 0 {
		t.Fatalf("WriteErrors = 0, want > 0 after database failure")
	}
}

func TestBodyQueueBudgetBoundsMemoryAndDrops(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 256 << 10
		c.MaxBodyStorageBytes = 32 << 20
		c.FlushInterval = time.Hour // writer batches but never flushes during the test
	})
	blob := make([]byte, 256<<10)
	for i := range blob {
		blob[i] = 'x'
	}
	accepted := 0
	for i := 0; i < 200; i++ {
		if s.Capture(pluginapi.RequestInterceptRequest{RequestID: "budget-" + time.Duration(i).String(), Body: blob}) {
			accepted++
		}
	}
	if accepted == 0 {
		t.Fatalf("no bodies accepted")
	}
	if accepted >= 200 {
		t.Fatalf("bounded budget did not reject any body: accepted=%d", accepted)
	}
	status := s.Status()
	if status.DroppedBodies == 0 {
		t.Errorf("DroppedBodies = 0, want > 0")
	}
	if status.Queued == 0 {
		t.Errorf("Queued = 0, want > 0")
	}
	if queued := s.queuedBodyBytes.Load(); queued > int64(200)*(256<<10) {
		t.Errorf("queued bytes unbounded: %d", queued)
	}
}
