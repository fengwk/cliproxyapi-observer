package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

const replayHorizon = 7 * 24 * time.Hour

type dedupEntry struct {
	Time  time.Time
	KeyID string
}

func (s *Store) loadCutoff(tx *bolt.Tx) error {
	m := tx.Bucket([]byte("meta"))
	var cutoff time.Time
	if b := m.Get([]byte("replay_cutoff")); b != nil {
		if e := decode(b, &cutoff); e != nil {
			return e
		}
	}
	candidate := s.now().UTC().Add(-replayHorizon)
	if candidate.After(cutoff) {
		cutoff = candidate
	}
	if s.cutoff.After(cutoff) {
		cutoff = s.cutoff
	}
	s.cutoff = cutoff
	return nil
}

func (s *Store) advanceCutoff(tx *bolt.Tx) error {
	if e := s.loadCutoff(tx); e != nil {
		return e
	}
	return put(tx.Bucket([]byte("meta")), "replay_cutoff", s.cutoff)
}

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
		s.lastError = "usage lookup failed"
		return fmt.Errorf("%w: %w", ErrAccounting, e)
	}
	if u.RequestID == "" {
		s.lastError = "invalid usage"
		return ErrAccounting
	}
	if seen {
		return nil
	}
	for _, n := range []int64{u.Detail.InputTokens, u.Detail.OutputTokens, u.Detail.ReasoningTokens, u.Detail.CachedTokens, u.Detail.CacheReadTokens, u.Detail.CacheCreationTokens, u.Detail.TotalTokens} {
		if n < 0 || n > maxSafe {
			s.lastError = "invalid usage"
			return ErrAccounting
		}
	}
	if u.RequestedAt.IsZero() || u.RequestedAt.Before(s.cutoff) || u.RequestedAt.After(s.now()) {
		s.lastError = "invalid usage"
		return ErrAccounting
	}
	r := normalize(u)
	at := s.now().UTC()
	s.pricer.Observe(r.Model, r.Provider)
	// Quote with the original request start and the final upstream tier, never
	// the ingestion time or a requested-tier preference.
	q, e := s.pricer.Quote(r.Model, r.Provider, r.Tokens, r.Time, r.ResponseServiceTier)
	if e != nil {
		s.lastError = "usage pricing failed"
		return fmt.Errorf("%w: %w", ErrAccounting, e)
	}
	// The pricing service represents an unavailable price or missing facts as
	// a zero quote for preview. It is not a calculable accounting result.
	if q.Source == "zero" || q.Reason == "missing_facts" || q.CostMicroUSD < 0 || q.CostMicroUSD > maxSafe {
		s.lastError = "usage pricing failed"
		return ErrAccounting
	}
	r.CostMicroUSD = q.CostMicroUSD
	r.PriceSource = q.Source
	if q.Rates.Reasoning != nil {
		rate := *q.Rates.Reasoning
		q.Rates.Reasoning = &rate
	}
	p := usageTransaction{Record: r, At: at, Quote: q, CaptureEnabled: s.settings.CaptureEnabled, CaptureMode: s.settings.CaptureMode, Epoch: key.Epoch}
	if e = s.persistLocked(p); e == nil {
		return nil
	}
	s.lastError = "usage persistence failed"
	return fmt.Errorf("%w: %w", ErrAccounting, e)
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
func (s *Store) persistLocked(p usageTransaction) error {
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
		// A reset window is charged according to the ingestion timestamp.
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
		// Minute buckets keep timezone-local day boundaries exact after raw
		// records and short-lived hourly rollups expire.
		if e := applyRollup(tx, "minutes", r, r.Time.UTC().Truncate(time.Minute)); e != nil {
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
