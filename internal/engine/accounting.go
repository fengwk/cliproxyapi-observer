package engine

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

func decode(b []byte, v any) error { return json.Unmarshal(b, v) }
func add(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrAccounting
	}
	return a + b, nil
}
func normalize(u pluginapi.UsageRecord) Record {
	d := u.Detail
	b := usage.EnsureTokenBreakdownForProvider(usage.Detail{InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, ReasoningTokens: d.ReasoningTokens, CachedTokens: d.CachedTokens, CacheReadTokens: d.CacheReadTokens, CacheCreationTokens: d.CacheCreationTokens, TotalTokens: d.TotalTokens}, u.Provider, u.ExecutorType).TokenBreakdown
	r := Record{ID: u.RequestID, TraceID: u.TraceID, KeyID: strings.TrimPrefix(u.APIKey, "observer:"), Time: u.RequestedAt.UTC(), Model: u.Model, Provider: u.Provider, AuthIndex: u.AuthIndex, Failed: u.Failed, StatusCode: u.Failure.StatusCode, Stream: u.Stream, LatencyMS: u.Latency.Milliseconds(), TTFTMS: u.TTFT.Milliseconds(), RawUsage: d, AccountingQuality: string(b.Quality)}
	r.Tokens.Total = b.TotalTokens
	if b.Quality == usage.TokenAccountingQualityComplete {
		r.Tokens = pricing.Tokens{Input: b.Input.UncachedTokens, Output: b.Output.TotalTokens, CacheRead: b.Input.CacheReadTokens, CacheWrite: b.Input.CacheWriteTokens, Reasoning: b.Output.ReasoningTokens, Total: b.TotalTokens}
	}
	return r
}
func (s *Store) fenceLocked() {
	s.fatal = true
	s.lastError = "irreversible accounting failure"
	s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("fence"), []byte("1")) })
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
	if u.RequestID == "" {
		s.fenceLocked()
		return ErrAccounting
	}
	var seen bool
	var key storedKey
	if e := s.db.View(func(tx *bolt.Tx) error {
		seen = tx.Bucket([]byte("dedup")).Get([]byte(u.RequestID)) != nil
		return get(tx.Bucket([]byte("keys")), strings.TrimPrefix(u.APIKey, "observer:"), &key)
	}); e != nil {
		if e == ErrNotFound {
			return e
		}
		s.fenceLocked()
		return ErrAccounting
	}
	if seen {
		return nil
	}
	if _, ok := s.pending[u.RequestID]; ok {
		return ErrAccounting
	}
	r := normalize(u)
	at := s.now().UTC()
	if r.Time.IsZero() {
		r.Time = at
	}
	s.pricer.Observe(r.Model, r.Provider)
	q, e := s.pricer.Quote(r.Model, r.Provider, r.Tokens, at)
	if e != nil || q.CostMicroUSD < 0 {
		s.fenceLocked()
		return ErrAccounting
	}
	r.CostMicroUSD = q.CostMicroUSD
	r.PriceSource = q.Source
	r.RuleID = q.RuleID
	if q.Rates.Reasoning != nil {
		rate := *q.Rates.Reasoning
		q.Rates.Reasoning = &rate
	}
	p := pending{Record: r, At: at, Quote: q, CaptureEnabled: s.settings.CaptureEnabled, Epoch: key.Epoch}
	if e = s.persistLocked(p); e == nil {
		return nil
	}
	s.blocked[r.KeyID] = true
	s.lastError = "usage persistence failed"
	if len(s.pending) >= 1024 {
		s.fenceLocked()
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
	var e error
	for _, p := range []struct {
		a *int64
		b int64
	}{{&b.Requests, 1}, {&b.CostMicroUSD, r.CostMicroUSD}, {&b.Tokens, r.Tokens.Total}} {
		*p.a, e = add(*p.a, p.b)
		if e != nil {
			return e
		}
	}
	if r.Failed {
		b.Failures, e = add(b.Failures, 1)
	}
	return e
}
func (s *Store) persistLocked(p pending) error {
	if s.writeFault != nil {
		if e := s.writeFault(); e != nil {
			return e
		}
	}
	return s.db.Update(func(tx *bolt.Tx) error {
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
		d := daily{KeyID: r.KeyID, Model: r.Model, Provider: r.Provider, AuthIndex: r.AuthIndex, Failed: r.Failed, Bucket: Bucket{Time: day(r.Time)}}
		db := tx.Bucket([]byte("daily"))
		id := dailyID(d)
		if v := db.Get([]byte(id)); v != nil {
			if e := decode(v, &d); e != nil {
				return e
			}
		}
		if e := accumulate(&d.Bucket, r); e != nil {
			return e
		}
		if e := put(kb, k.ID, k); e != nil {
			return e
		}
		if e := put(db, id, d); e != nil {
			return e
		}
		if e := put(tx.Bucket([]byte("records")), r.ID, recordData{Record: r, Quote: p.Quote, CaptureEnabled: p.CaptureEnabled}); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("timeline")).Put([]byte(timeID(r)), []byte(r.ID)); e != nil {
			return e
		}
		return tx.Bucket([]byte("dedup")).Put([]byte(r.ID), []byte(r.KeyID))
	})
}
func (s *Store) retryLocked() error {
	for id, p := range s.pending {
		if e := s.persistLocked(p); e != nil {
			return ErrAccounting
		}
		delete(s.pending, id)
	}
	clear(s.blocked)
	if !s.fatal {
		s.lastError = ""
	}
	return nil
}
