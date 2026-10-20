package engine

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	bolt "go.etcd.io/bbolt"
)

const (
	statsVersion         = 1
	statsBackfillBatch   = 1000
	statsBackfillDoneKey = "stats_backfill_done"
	statsBackfillCursor  = "stats_backfill_cursor"
	statsBackfillVersion = "stats_backfill_version"
)

// addAggregate is the single overflow-checked merge used by writes, backfill
// and reads. A failure aborts the enclosing bbolt transaction.
func addAggregate(dst *Aggregate, src Aggregate) error {
	pairs := []struct {
		a *int64
		b int64
	}{
		{&dst.Requests, src.Requests},
		{&dst.Failures, src.Failures},
		{&dst.CostMicroUSD, src.CostMicroUSD},
		{&dst.Tokens, src.Tokens},
		{&dst.InputTokens, src.InputTokens},
		{&dst.OutputTokens, src.OutputTokens},
		{&dst.CacheReadTokens, src.CacheReadTokens},
		{&dst.CacheWriteTokens, src.CacheWriteTokens},
		{&dst.ReasoningTokens, src.ReasoningTokens},
		{&dst.CompleteRequests, src.CompleteRequests},
		{&dst.LatencyMS, src.LatencyMS},
		{&dst.LatencySamples, src.LatencySamples},
		{&dst.TTFTMS, src.TTFTMS},
		{&dst.TTFTSamples, src.TTFTSamples},
		{&dst.UnpricedRequests, src.UnpricedRequests},
		{&dst.DetailedRequests, src.DetailedRequests},
	}
	for _, p := range pairs {
		n, e := add(*p.a, p.b)
		if e != nil {
			return e
		}
		*p.a = n
	}
	return nil
}

// detailedAggregate contains only fields introduced by the detailed statistics
// version. Backfill must not add the original four totals again.
func detailedAggregate(r Record) Aggregate {
	a := Aggregate{
		DetailedRequests: 1,
	}
	if r.AccountingQuality == string(usage.TokenAccountingQualityComplete) {
		a.CompleteRequests = 1
		a.InputTokens = r.Tokens.Input
		a.OutputTokens = r.Tokens.Output
		a.CacheReadTokens = r.Tokens.CacheRead
		a.CacheWriteTokens = r.Tokens.CacheWrite
		a.ReasoningTokens = r.Tokens.Reasoning
	}
	if r.LatencyMS > 0 {
		a.LatencyMS = r.LatencyMS
		a.LatencySamples = 1
	}
	if r.Stream && r.LatencyMS > 0 && r.TTFTMS > 0 && r.TTFTMS < r.LatencyMS {
		a.TTFTMS = r.TTFTMS
		a.TTFTSamples = 1
	}
	if r.PriceSource == "unpriced" {
		a.UnpricedRequests = 1
	}
	return a
}

func recordAggregate(r Record) Aggregate {
	a := detailedAggregate(r)
	a.Requests = 1
	a.Tokens = r.Tokens.Total
	a.CostMicroUSD = r.CostMicroUSD
	if r.Failed {
		a.Failures = 1
	}
	return a
}

func rollupDaily(r Record, at time.Time) daily {
	return daily{KeyID: r.KeyID, Model: r.Model, Provider: r.Provider, AuthIndex: r.AuthIndex, Failed: r.Failed, Bucket: Bucket{Time: at}}
}

func applyRollup(tx *bolt.Tx, bucket string, r Record, at time.Time) error {
	d := rollupDaily(r, at)
	b := tx.Bucket([]byte(bucket))
	id := dailyID(d)
	if raw := b.Get([]byte(id)); raw != nil {
		if e := decode(raw, &d); e != nil {
			return e
		}
	}
	if e := addAggregate(&d.Bucket.Aggregate, recordAggregate(r)); e != nil {
		return e
	}
	return put(b, id, d)
}

// backfillDailyDetails adds only detailed fields to an existing daily rollup;
// the original four totals are already present from the old writer.
func backfillDailyDetails(tx *bolt.Tx, r Record) error {
	d := rollupDaily(r, day(r.Time))
	b := tx.Bucket([]byte("daily"))
	id := dailyID(d)
	raw := b.Get([]byte(id))
	if raw == nil {
		return nil
	}
	if e := decode(raw, &d); e != nil {
		return e
	}
	if e := addAggregate(&d.Bucket.Aggregate, detailedAggregate(r)); e != nil {
		return e
	}
	return put(b, id, d)
}

func backfillHourly(tx *bolt.Tx, r Record) error {
	d := rollupDaily(r, r.Time.UTC().Truncate(time.Hour))
	b := tx.Bucket([]byte("hourly"))
	id := dailyID(d)
	if raw := b.Get([]byte(id)); raw != nil {
		if e := decode(raw, &d); e != nil {
			return e
		}
	}
	if e := addAggregate(&d.Bucket.Aggregate, recordAggregate(r)); e != nil {
		return e
	}
	return put(b, id, d)
}

// backfillStatistics backfills detailed fields for raw rows that are still
// present. It runs in checkpointed batches before the store starts serving new
// writes, so restart resumes exactly after the last committed cursor. The
// per-record version makes the operation idempotent even if a caller replays a
// batch or a future maintenance pass revisits it.
func (s *Store) backfillStatistics() error {
	var done bool
	if e := s.db.View(func(tx *bolt.Tx) error {
		done = tx.Bucket([]byte("meta")).Get([]byte(statsBackfillDoneKey)) != nil
		return nil
	}); e != nil || done {
		return e
	}
	for {
		var finished bool
		e := s.db.Update(func(tx *bolt.Tx) error {
			m := tx.Bucket([]byte("meta"))
			b := tx.Bucket([]byte("records"))
			c := b.Cursor()
			var k, v []byte
			if cursor := m.Get([]byte(statsBackfillCursor)); cursor == nil {
				k, v = c.First()
			} else {
				k, v = c.Seek(cursor)
			}
			processed := 0
			for k != nil && processed < statsBackfillBatch {
				var stored recordData
				if e := decode(v, &stored); e != nil {
					return e
				}
				if stored.StatsVersion < statsVersion {
					r := stored.Record
					if e := backfillDailyDetails(tx, r); e != nil {
						return e
					}
					if e := backfillHourly(tx, r); e != nil {
						return e
					}
					stored.StatsVersion = statsVersion
					if e := put(b, string(k), stored); e != nil {
						return e
					}
				}
				processed++
				k, v = c.Next()
			}
			if k == nil {
				if e := m.Put([]byte(statsBackfillDoneKey), []byte("1")); e != nil {
					return e
				}
				if e := m.Put([]byte(statsBackfillVersion), []byte("1")); e != nil {
					return e
				}
				if e := m.Delete([]byte(statsBackfillCursor)); e != nil {
					return e
				}
				finished = true
				return nil
			}
			if e := m.Put([]byte(statsBackfillCursor), append([]byte(nil), k...)); e != nil {
				return e
			}
			return nil
		})
		if e != nil {
			return e
		}
		if finished {
			return nil
		}
	}
}

// rollupScanStart returns a lexicographic lower bound for dailyID-style keys at
// or after t. Rollup keys begin with a JSON string-encoded UTC timestamp.
func rollupScanStart(t time.Time) []byte {
	return []byte(`["` + t.UTC().Format(time.RFC3339Nano))
}

func scanRollups(b *bolt.Bucket, from, to time.Time, fn func(daily) error) error {
	if b == nil || !to.After(from) {
		return nil
	}
	c := b.Cursor()
	for k, v := c.Seek(rollupScanStart(from)); k != nil; k, v = c.Next() {
		var d daily
		if e := decode(v, &d); e != nil {
			return e
		}
		if !d.Bucket.Time.Before(to) {
			break
		}
		if d.Bucket.Time.Before(from) {
			continue
		}
		if e := fn(d); e != nil {
			return e
		}
	}
	return nil
}
