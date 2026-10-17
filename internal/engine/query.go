package engine

import (
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

func validQuery(q Query) bool {
	return !q.From.IsZero() && q.To.After(q.From) && q.Page >= 0 && q.PageSize >= 0 && q.PageSize <= 100 && (q.Status == "" || q.Status == "success" || q.Status == "failed")
}
func matches(q Query, key, model, provider, auth string, failed bool) bool {
	return (q.KeyID == "" || q.KeyID == key) && (q.Model == "" || q.Model == model) && (q.Provider == "" || q.Provider == provider) && (q.AuthIndex == "" || q.AuthIndex == auth) && (q.Status == "" || (q.Status == "failed") == failed)
}
func (s *Store) Requests(q Query) (RequestPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := RequestPage{Items: []Record{}, Page: q.Page, PageSize: q.PageSize}
	if out.Page == 0 {
		out.Page = 1
	}
	if out.PageSize == 0 {
		out.PageSize = 25
	}
	if e := s.check(); e != nil {
		return out, e
	}
	if !validQuery(q) {
		return out, ErrInvalid
	}
	if out.Page > int(^uint(0)>>1)/out.PageSize {
		return out, ErrInvalid
	}
	skip := (out.Page - 1) * out.PageSize
	cut := s.now().Add(-time.Duration(s.settings.RawRetentionHours) * time.Hour)
	e := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("timeline")).Cursor()
		k, v := c.Seek([]byte(timePrefix(q.To)))
		if k == nil {
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
		for ; k != nil; k, v = c.Prev() {
			var r Record
			if e := get(tx.Bucket([]byte("records")), string(v), &r); e != nil {
				return e
			}
			if r.Time.Before(cut) || r.Time.Before(q.From) {
				break
			}
			if !r.Time.Before(q.To) || !matches(q, r.KeyID, r.Model, r.Provider, r.AuthIndex, r.Failed) {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			if len(out.Items) == out.PageSize {
				out.HasMore = true
				break
			}
			out.Items = append(out.Items, r)
		}
		return nil
	})
	return out, e
}
func (s *Store) Summary(q Query) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Summary{Granularity: "day", Buckets: []Bucket{}}
	if e := s.check(); e != nil {
		return out, e
	}
	if !validQuery(q) {
		return out, ErrInvalid
	}
	cut := s.now().Add(-time.Duration(s.settings.RawRetentionHours) * time.Hour)
	if (q.From.Before(cut) && !q.From.Equal(day(q.From))) || (q.From.Before(cut) && day(q.To).Before(cut) && !q.To.Equal(day(q.To))) {
		return out, ErrInvalid
	}
	bs := map[time.Time]*Bucket{}
	merge := func(b Bucket) error {
		v := bs[b.Time]
		if v == nil {
			v = &Bucket{Time: b.Time}
			bs[b.Time] = v
		}
		for _, p := range []struct {
			a *int64
			b int64
		}{{&v.Requests, b.Requests}, {&v.Failures, b.Failures}, {&v.CostMicroUSD, b.CostMicroUSD}, {&v.Tokens, b.Tokens}} {
			n, e := add(*p.a, p.b)
			if e != nil {
				return e
			}
			*p.a = n
		}
		return nil
	}
	dailyCut := day(s.now()).AddDate(0, 0, -s.settings.DailyRetentionDays)
	full := func(t time.Time) bool {
		return !t.Before(q.From) && !t.AddDate(0, 0, 1).After(q.To) && !t.Before(dailyCut)
	}
	e := s.db.View(func(tx *bolt.Tx) error {
		if e := tx.Bucket([]byte("daily")).ForEach(func(_, v []byte) error {
			var d daily
			if e := decode(v, &d); e != nil {
				return e
			}
			if full(d.Bucket.Time) && matches(q, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed) {
				return merge(d.Bucket)
			}
			return nil
		}); e != nil {
			return e
		}
		return tx.Bucket([]byte("records")).ForEach(func(_, v []byte) error {
			var r Record
			if e := decode(v, &r); e != nil {
				return e
			}
			if full(day(r.Time)) || r.Time.Before(cut) || r.Time.Before(q.From) || !r.Time.Before(q.To) || !matches(q, r.KeyID, r.Model, r.Provider, r.AuthIndex, r.Failed) {
				return nil
			}
			b := Bucket{Time: day(r.Time)}
			if e := accumulate(&b, r); e != nil {
				return e
			}
			return merge(b)
		})
	})
	if e != nil {
		return out, e
	}
	for _, b := range bs {
		out.Buckets = append(out.Buckets, *b)
		for _, p := range []struct {
			a *int64
			b int64
		}{{&out.Requests, b.Requests}, {&out.Failures, b.Failures}, {&out.CostMicroUSD, b.CostMicroUSD}, {&out.Tokens, b.Tokens}} {
			n, e := add(*p.a, p.b)
			if e != nil {
				return out, e
			}
			*p.a = n
		}
	}
	sort.Slice(out.Buckets, func(i, j int) bool { return out.Buckets[i].Time.Before(out.Buckets[j].Time) })
	return out, nil
}

func timePrefix(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func timeID(r Record) string        { return timePrefix(r.Time) + r.ID }
