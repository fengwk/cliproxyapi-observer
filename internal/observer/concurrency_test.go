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
