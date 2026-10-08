package observer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentSubmitCaptureFlushRead exercises the bounded writer with
// concurrent producers, capturers, readers and flushers. Run under -race and
// with -count to repeat.
func TestConcurrentSubmitCaptureFlushRead(t *testing.T) {
	for iter := 0; iter < 3; iter++ {
		func() {
			s := openTestStore(t, func(c *Config) {
				c.CaptureBodies = true
				c.FlushInterval = 5 * time.Millisecond
			})

			const (
				writers     = 8
				perWriter   = 200
				capturers   = 4
				perCapture  = 100
				readers     = 4
				flushRounds = 50
			)

			var accepted atomic.Int64
			var producers sync.WaitGroup
			stopReaders := make(chan struct{})

			var bodyMu sync.Mutex
			acceptedBodies := make([]string, 0, capturers*perCapture)

			for w := 0; w < writers; w++ {
				producers.Add(1)
				go func(w int) {
					defer producers.Done()
					for i := 0; i < perWriter; i++ {
						id := fmt.Sprintf("c-%d-%d", w, i)
						at := time.Now().Add(-time.Duration(perWriter-i) * time.Millisecond)
						if s.SubmitUsage(usageRecord(id, "openai", "gpt-5", at, simpleUsage(10, 5))) {
							accepted.Add(1)
						}
					}
				}(w)
			}
			for c := 0; c < capturers; c++ {
				producers.Add(1)
				go func(c int) {
					defer producers.Done()
					for i := 0; i < perCapture; i++ {
						id := fmt.Sprintf("b-%d-%d", c, i)
						if captureBody(s, id, `{"n":`+fmt.Sprint(i)+`}`) {
							bodyMu.Lock()
							acceptedBodies = append(acceptedBodies, id)
							bodyMu.Unlock()
						}
					}
				}(c)
			}
			for f := 0; f < 2; f++ {
				producers.Add(1)
				go func() {
					defer producers.Done()
					for i := 0; i < flushRounds; i++ {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						_ = s.Flush(ctx)
						cancel()
					}
				}()
			}

			var readerWG sync.WaitGroup
			for r := 0; r < readers; r++ {
				readerWG.Add(1)
				go func() {
					defer readerWG.Done()
					ctx := context.Background()
					for {
						select {
						case <-stopReaders:
							return
						default:
						}
						_, _ = s.Requests(Query{Limit: 20})
						_, _ = s.Summary(Query{})
						_, _ = s.Body("c-0-0")
						_ = s.Status()
						_ = s.Flush(ctx)
					}
				}()
			}

			producers.Wait()
			close(stopReaders)
			readerWG.Wait()

			flushAll(t, s)

			total := 0
			query := Query{Limit: 100}
			for {
				page, err := s.Requests(query)
				if err != nil {
					t.Fatalf("Requests: %v", err)
				}
				total += len(page.Items)
				if !page.HasMore {
					break
				}
				query.Cursor = page.NextCursor
			}
			if int64(total) != accepted.Load() {
				t.Fatalf("stored %d records but %d were accepted", total, accepted.Load())
			}
			for _, id := range acceptedBodies {
				if _, err := s.Body(id); err != nil {
					t.Fatalf("body %s missing: %v", id, err)
				}
			}
		}()
	}
}

// TestCloseRacesReadersWithMultipleClosers ensures Close is idempotent and
// never lets concurrent readers observe a panicking/raced database.
func TestCloseRacesReadersWithMultipleClosers(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
		at := time.Now().Add(-time.Minute)
		s.SubmitUsage(usageRecord("close-1", "openai", "gpt-5", at, simpleUsage(1, 1)))
		captureBody(s, "close-1", `{"a":1}`)
		flushAll(t, s)

		var closers sync.WaitGroup
		errs := make([]error, 4)
		for i := 0; i < 4; i++ {
			closers.Add(1)
			go func(i int) {
				defer closers.Done()
				errs[i] = s.Close()
			}(i)
		}

		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, err := s.Requests(Query{}); errors.Is(err, ErrClosed) {
				break
			}
			_, _ = s.Summary(Query{})
			_, _ = s.Body("close-1")
			_ = s.Status()
		}
		closers.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("closer %d: %v", i, err)
			}
		}
	}
}

// TestClosePersistsEveryAcceptedObservation verifies the close race fix: no
// successful Submit may be enqueued after the writer's final drain, so every
// accepted usage record must survive a concurrent Close.
func TestClosePersistsEveryAcceptedObservation(t *testing.T) {
	const writers = 5
	const perWriter = 200
	for iter := 0; iter < 5; iter++ {
		cfg := testConfig(t, func(c *Config) { c.FlushInterval = 5 * time.Millisecond })
		s, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		var accepted atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				for i := 0; i < perWriter; i++ {
					id := fmt.Sprintf("p-%d-%d-%d", iter, w, i)
					at := time.Now().Add(-time.Duration(i) * time.Millisecond)
					if s.SubmitUsage(usageRecord(id, "openai", "gpt-5", at, simpleUsage(1, 1))) {
						accepted.Add(1)
					}
				}
			}(w)
		}
		close(start)
		time.Sleep(time.Millisecond) // let producers get going
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		wg.Wait()

		// After Close no further submissions may be accepted.
		if s.SubmitUsage(usageRecord("after-close", "openai", "gpt-5", time.Now(), simpleUsage(1, 1))) {
			t.Fatalf("Submit accepted after Close")
		}

		reopened, err := Open(cfg)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		total := len(allRequests(t, reopened))
		if err := reopened.Close(); err != nil {
			t.Fatalf("reopened Close: %v", err)
		}
		if int64(total) != accepted.Load() {
			t.Fatalf("iter %d: accepted %d but persisted %d", iter, accepted.Load(), total)
		}
	}
}

// TestFlushReturnsWhileProducersActive verifies the bounded drain: Flush must
// return even while a producer keeps the queue busy (no unbounded collection).
func TestFlushReturnsWhileProducersActive(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.FlushInterval = time.Hour })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.SubmitUsage(usageRecord(fmt.Sprintf("spin-%d", i), "openai", "gpt-5", time.Now(), simpleUsage(1, 1)))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Flush(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Flush did not return while producers were active")
	}
	close(stop)
	wg.Wait()
	flushAll(t, s)
}

// TestCloseReportsWriteFailure verifies Close surfaces a flush/database failure
// instead of claiming success.
func TestCloseReportsWriteFailure(t *testing.T) {
	s := openTestStore(t, nil)
	at := time.Now().Add(-time.Minute)
	s.SubmitUsage(usageRecord("cw-1", "openai", "gpt-5", at, simpleUsage(1, 1)))
	flushAll(t, s)

	if err := s.db.Close(); err != nil {
		t.Fatalf("force close: %v", err)
	}
	s.SubmitUsage(usageRecord("cw-2", "openai", "gpt-5", at, simpleUsage(1, 1)))
	if err := s.Close(); err == nil {
		t.Fatalf("Close reported success after a write failure")
	}
	if s.Status().WriteErrors == 0 {
		t.Errorf("WriteErrors = 0, want > 0")
	}
}
