package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

func add(a, b int64) (int64, error) {
	if a < 0 || b < 0 || b > maxSafe || a > maxSafe-b {
		return 0, ErrAccounting
	}
	return a + b, nil
}
func normalize(u pluginapi.UsageRecord) Record {
	d := u.Detail
	b := canonicalTokenBreakdown(d, u.Provider, u.ExecutorType)
	r := Record{ID: u.RequestID, TraceID: u.TraceID, KeyID: strings.TrimPrefix(u.APIKey, "observer:"), Time: u.RequestedAt.UTC(), Model: u.Model, Provider: u.Provider, AuthIndex: u.AuthIndex, Failed: u.Failed, StatusCode: u.Failure.StatusCode, Stream: u.Stream, LatencyMS: u.Latency.Milliseconds(), TTFTMS: u.TTFT.Milliseconds(), RawUsage: d, ResponseServiceTier: u.ResponseServiceTier, AccountingQuality: string(b.Quality)}
	r.Tokens.Total = b.TotalTokens
	if b.Quality == usage.TokenAccountingQualityComplete {
		r.Tokens = pricing.Tokens{Input: b.Input.UncachedTokens, Output: b.Output.TotalTokens, CacheRead: b.Input.CacheReadTokens, CacheWrite: b.Input.CacheWriteTokens, Reasoning: b.Output.ReasoningTokens, Total: b.TotalTokens}
	}
	return r
}
func (s *Store) RecordUsage(u pluginapi.UsageRecord) error {
	if !u.Generate || !strings.HasPrefix(u.APIKey, "observer:") {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	var seen bool
	var key storedKey
	if e := s.db.View(func(tx *bolt.Tx) error {
		if e := s.loadCutoff(tx); e != nil {
			return e
		}
		seen = tx.Bucket([]byte("dedup")).Get([]byte(u.RequestID)) != nil
		return get(tx.Bucket([]byte("keys")), strings.TrimPrefix(u.APIKey, "observer:"), &key)
	}); e != nil {
		if e == ErrNotFound {
			return e
		}
		s.fenceLocked(strings.TrimPrefix(u.APIKey, "observer:"))
		return ErrAccounting
	}
	if u.RequestID == "" {
		s.fenceLocked(key.ID)
		return ErrAccounting
	}
	if seen {
		return nil
	}
	if _, ok := s.pending[u.RequestID]; ok {
		return ErrAccounting
	}
	if u.RequestedAt.IsZero() || u.RequestedAt.Before(s.cutoff) || u.RequestedAt.After(s.now()) {
		s.fenceLocked(key.ID)
		return ErrAccounting
	}
	r := normalize(u)
	at := s.now().UTC()
	s.pricer.Observe(r.Model, r.Provider)
	// Quote with the original request start and the final upstream tier, never
	// the ingestion time or a requested-tier preference.
	q, e := s.pricer.Quote(r.Model, r.Provider, r.Tokens, r.Time, r.ResponseServiceTier)
	if e != nil || q.CostMicroUSD < 0 || q.CostMicroUSD > maxSafe {
		s.fenceLocked(strings.TrimPrefix(u.APIKey, "observer:"))
		return ErrAccounting
	}
	r.CostMicroUSD = q.CostMicroUSD
	r.PriceSource = q.Source
	if q.Rates.Reasoning != nil {
		rate := *q.Rates.Reasoning
		q.Rates.Reasoning = &rate
	}
	p := pending{Record: r, At: at, Quote: q, CaptureEnabled: s.settings.CaptureEnabled, CaptureMode: s.settings.CaptureMode, Epoch: key.Epoch}
	if e = s.persistLocked(p); e == nil {
		return nil
	}
	if errors.Is(e, ErrAccounting) {
		s.fenceLocked(r.KeyID)
		return ErrAccounting
	}
	s.blocked[r.KeyID] = true
	s.lastError = "usage persistence failed"
	if len(s.pending) >= 1024 {
		s.fenceLocked(strings.TrimPrefix(u.APIKey, "observer:"))
		return ErrAccounting
	}
	s.pending[r.ID] = p
	return ErrAccounting
}

// Daily dimensions are explicit JSON values; no delimiter collision can merge
// two models, principals, providers or auth indices.
type daily struct {
	KeyID, Model, Provider, AuthIndex string
	Failed                            bool
	Bucket                            Bucket
}

func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
func dailyID(d daily) string {
	p, _ := json.Marshal([]any{d.Bucket.Time, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed})
	return string(p)
}
func accumulate(b *Bucket, r Record) error {
	return addAggregate(&b.Aggregate, recordAggregate(r))
}
func (s *Store) persistLocked(p pending) error {
	if s.usageFault != nil {
		if e := s.usageFault(p); e != nil {
			return e
		}
	}
	if s.writeFault != nil {
		if e := s.writeFault(); e != nil {
			return e
		}
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if e := s.advanceCutoff(tx); e != nil {
			return e
		}
		r := p.Record
		if tx.Bucket([]byte("dedup")).Get([]byte(r.ID)) != nil {
			return nil
		}
		var k storedKey
		kb := tx.Bucket([]byte("keys"))
		if e := get(kb, r.KeyID, &k); e != nil {
			return e
		}
		// Retries retain their original ingestion timestamp. Events older than an
		// explicit reset never refill that reset window.
		roll(&k.Key, p.At)
		for _, v := range []struct {
			used          *int64
			start         time.Time
			epoch, quoted int64
		}{{&k.Used.FiveHour, k.Starts.FiveHour, k.Epoch.FiveHour, p.Epoch.FiveHour}, {&k.Used.Week, k.Starts.Week, k.Epoch.Week, p.Epoch.Week}, {&k.Used.Month, k.Starts.Month, k.Epoch.Month, p.Epoch.Month}, {&k.Used.Total, k.Starts.Total, k.Epoch.Total, p.Epoch.Total}} {
			if v.epoch == v.quoted && !p.At.Before(v.start) {
				n, e := add(*v.used, r.CostMicroUSD)
				if e != nil {
					return e
				}
				*v.used = n
			}
		}
		if e := put(kb, k.ID, k); e != nil {
			return e
		}
		if e := applyRollup(tx, "daily", r, day(r.Time)); e != nil {
			return e
		}
		if e := applyRollup(tx, "hourly", r, r.Time.UTC().Truncate(time.Hour)); e != nil {
			return e
		}
		if e := put(tx.Bucket([]byte("records")), r.ID, recordData{Record: r, Quote: p.Quote, CaptureEnabled: p.CaptureEnabled, CaptureMode: p.CaptureMode, StatsVersion: statsVersion}); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("timeline")).Put([]byte(timeID(r)), []byte(r.ID)); e != nil {
			return e
		}
		cb := tx.Bucket([]byte("correlations"))
		cid := captureID(r.KeyID, r.TraceID)
		if value := cb.Get([]byte(cid)); value != nil {
			var c correlation
			if e := decode(value, &c); e != nil {
				return e
			}
			if p.At.After(c.At) {
				c.At = p.At
			}
			if e := put(cb, cid, c); e != nil {
				return e
			}
		}
		return put(tx.Bucket([]byte("dedup")), r.ID, dedupEntry{Time: r.Time, KeyID: r.KeyID})
	})
}
func (s *Store) retryLocked() error {
	var result error
	for id, p := range s.pending {
		if e := s.persistLocked(p); e != nil {
			result = ErrAccounting
			if errors.Is(e, ErrAccounting) {
				s.fenceLocked(p.Record.KeyID)
				delete(s.pending, id)
			}
			continue
		}
		delete(s.pending, id)
	}
	clear(s.blocked)
	for _, p := range s.pending {
		s.blocked[p.Record.KeyID] = true
	}
	if result != nil {
		s.lastError = "usage persistence failed"
	} else if len(s.unknown) == 0 {
		s.lastError = ""
	} else {
		s.lastError = unknownReason
	}
	return result
}
