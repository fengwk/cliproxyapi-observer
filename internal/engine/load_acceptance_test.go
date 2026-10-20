//go:build load

package engine

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
)

func percentiles(samples []time.Duration) (time.Duration, time.Duration) {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2], samples[(len(samples)*95-1)/100]
}

// Explicit, repeatable load acceptance (not part of normal unit tests). Use
// actual gzip records, full-year dimensions, simultaneous admission, three
// near-full capture budgets and real maintenance. Timing limits are the 5s UI
// refresh period, not unverified microsecond promises or production guarantees.
func TestQueryAdmissionAndCaptureLoad(t *testing.T) {
	for _, count := range []int{10000, 100000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, k, now := seedReadLoad(t, count)
			var wg sync.WaitGroup
			waits := make([]time.Duration, 0, 100)
			lockWaits := make([]time.Duration, 0, 100)
			errs := make(chan error, 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					lockStart := time.Now()
					s.mu.Lock()
					lockWaits = append(lockWaits, time.Since(lockStart))
					s.mu.Unlock()
					start := time.Now()
					err := s.Admit(Admission{RequestID: fmt.Sprintf("admit-%d", i), KeyID: k.ID, Model: "model-000", At: now})
					waits = append(waits, time.Since(start))
					if err == nil {
						err = s.Complete(fmt.Sprintf("admit-%d", i), now)
					}
					if err != nil {
						errs <- err
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			durations := make([]time.Duration, 0, 20)
			for i := 0; i < 20; i++ {
				start := time.Now()
				v, err := s.RequestView(ViewQuery{FromDate: "2025-10-20", ToDate: "2026-10-20", View: viewProvider})
				durations = append(durations, time.Since(start))
				if err != nil || v.Summary.Requests != 36500000+int64(count) {
					t.Fatal(v.Summary, err)
				}
			}
			wg.Wait()
			select {
			case err := <-errs:
				t.Fatal(err)
			default:
			}
			q50, q95 := percentiles(durations)
			a50, a95 := percentiles(waits)
			l50, l95 := percentiles(lockWaits)
			t.Logf("records=%d daily_dimensions=36500 samples=20 query_p50=%v query_p95=%v admission_p50=%v admission_p95=%v store_lock_p50=%v store_lock_p95=%v",
				count, q50, q95, a50, a95, l50, l95)
			if q95 >= 5*time.Second || a95 >= 5*time.Second {
				t.Fatal("queries/admission exceed the refresh budget")
			}
			// Deterministic high-entropy JSON: compression cannot hide pressure by
			// turning a repeated-string fixture into a tiny row.
			rng := rand.New(rand.NewSource(1))
			payload := make([]byte, 65536)
			for i := range payload {
				payload[i] = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"[rng.Intn(62)]
			}
			body := append(append([]byte(`{"prompt":"`), payload...), []byte(`"}`)...)
			for _, budget := range []int64{1 << 20, 8 << 20, 32 << 20} {
				configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.MaxBodyStorageBytes = budget })
				// Push beyond capacity to exercise FIFO with the indexed byte
				// counter. Drain each accepted write; queue contention is tested
				// separately by the unit/race suite.
				for i := 0; i < int(budget/48000)+5; i++ {
					id := fmt.Sprintf("content-%d-%d", budget, i)
					if !s.Capture(CaptureInput{RequestID: id, TraceID: id, KeyID: k.ID, At: now, Body: body}) {
						t.Fatal("serial load capture rejected")
					}
					drainCapture(s)
				}
				h := s.Health()
				if h.CaptureBytes > budget || h.CaptureBytes < budget*9/10 {
					t.Fatal("budget counter or near-full fixture invalid", h)
				}
				if err := s.Compact(); err != nil {
					t.Fatal("idle compaction under load", err)
				}
				t.Logf("budget=%d capture_bytes=%d database_bytes=%d drops=%v", budget, h.CaptureBytes, h.DatabaseBytes, h.CaptureDrops)
				if _, err := s.PurgeCaptures(); err != nil {
					t.Fatal(err)
				}
			}
			v, err := s.RequestView(ViewQuery{})
			if err != nil || v.Summary.Requests != int64(count) || v.Summary.CostMicroUSD != 7*int64(count) {
				t.Fatal("content/maintenance changed immutable totals", v.Summary, err)
			}
		})
	}
}
