package engine

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	viewRequests = "requests"
	viewModel    = "model"
	viewKey      = "key"
	viewKeyModel = "key_model"
	viewAccount  = "account"
	viewProvider = "provider"

	facetModel     = "model"
	facetKeyID     = "key_id"
	facetProvider  = "provider"
	facetAuthIndex = "auth_index"
)

func validQuery(q Query) bool {
	return !q.From.IsZero() && q.To.After(q.From) && q.Page >= 0 && q.PageSize >= 0 && q.PageSize <= 100 && (q.Status == "" || q.Status == "success" || q.Status == "failed")
}
func matches(q Query, key, model, provider, auth string, failed bool) bool {
	return (q.KeyID == "" || q.KeyID == key) && (q.Model == "" || q.Model == model) && (q.Provider == "" || q.Provider == provider) && (q.AuthIndex == "" || q.AuthIndex == auth) && (q.Status == "" || (q.Status == "failed") == failed)
}

// queryEnv is a short-lived snapshot of the immutable query configuration and
// a database read lease. s.mu is only held while taking it; the actual scan
// never blocks admission, accounting or physical-maintenance bookkeeping.
type queryEnv struct {
	db        *bolt.DB
	now       time.Time
	rawCut    time.Time
	dailyCut  time.Time
	rawHours  int
	dailyDays int
}

func (s *Store) queryEnv() (*queryEnv, func(), error) {
	s.mu.Lock()
	if e := s.check(); e != nil {
		s.mu.Unlock()
		return nil, nil, e
	}
	// Lock order is s.mu -> dbMu. Close and Compact take dbMu only while
	// holding s.mu, so the handle stays valid for the entire read transaction.
	s.dbMu.RLock()
	db := s.db
	env := &queryEnv{
		db:        db,
		now:       s.now().UTC(),
		rawHours:  s.settings.RawRetentionHours,
		dailyDays: s.settings.DailyRetentionDays,
	}
	env.rawCut = env.now.Add(-time.Duration(env.rawHours) * time.Hour)
	env.dailyCut = day(env.now).AddDate(0, 0, -env.dailyDays)
	s.mu.Unlock()
	return env, s.dbMu.RUnlock, nil
}

func pageLimits(page, size, max int) (int, int, bool) {
	if page < 0 || size < 0 {
		return 0, 0, false
	}
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 25
	}
	if size > max || page > int(^uint(0)>>1)/size {
		return 0, 0, false
	}
	return page, size, true
}

func (s *Store) Requests(q Query) (RequestPage, error) {
	out := RequestPage{Items: []Record{}, Page: q.Page, PageSize: q.PageSize}
	if out.Page == 0 {
		out.Page = 1
	}
	if out.PageSize == 0 {
		out.PageSize = 25
	}
	env, release, e := s.queryEnv()
	if e != nil {
		return out, e
	}
	defer release()
	if !validQuery(q) {
		return out, ErrInvalid
	}
	if out.Page > int(^uint(0)>>1)/out.PageSize {
		return out, ErrInvalid
	}
	e = env.db.View(func(tx *bolt.Tx) error {
		items, more, e := scanRequestPage(tx, env, q, out.Page, out.PageSize)
		out.Items, out.HasMore = items, more
		return e
	})
	return out, e
}

func (s *Store) Summary(q Query) (Summary, error) {
	out := Summary{Granularity: "day", Buckets: []Bucket{}}
	env, release, e := s.queryEnv()
	if e != nil {
		return out, e
	}
	defer release()
	if !validQuery(q) {
		return out, ErrInvalid
	}
	granularity, e := summaryGranularity(env, q)
	if e != nil {
		return out, e
	}
	out.Granularity = granularity
	e = env.db.View(func(tx *bolt.Tx) error {
		agg, buckets, _, e := collect(tx, env, q, granularity, viewRequests, nil)
		if e != nil {
			return e
		}
		out.Aggregate = agg
		out.Buckets = sortedBuckets(buckets)
		return nil
	})
	return out, e
}

// RequestView returns the summary, trend and one current view inside one read
// transaction. Date-only inputs are inclusive calendar dates in the requested
// IANA timezone; the end is clamped to the query instant so a "today" range
// never includes future buckets. UTC retains the existing daily/hourly fast
// paths; other zones group retained minute aggregates into local days or
// elapsed-hour buckets so region boundaries survive raw-record expiry.
func (s *Store) RequestView(v ViewQuery) (RequestView, error) {
	env, release, e := s.queryEnv()
	if e != nil {
		return RequestView{}, e
	}
	defer release()
	q, view, zone, loc, e := env.normalizeView(v)
	if e != nil {
		return RequestView{}, e
	}
	out := RequestView{
		Range:   ViewRange{From: q.From, To: q.To, AsOf: env.now, Timezone: zone},
		Series:  Series{Granularity: "day", Buckets: []Bucket{}},
		Records: []Record{}, Groups: []Group{},
		Pagination: Pagination{Page: q.Page, PageSize: q.PageSize},
	}
	var granularity string
	if loc == time.UTC {
		granularity, e = summaryGranularity(env, q)
		if e != nil {
			return out, e
		}
	} else if q.From.Before(env.rawCut) {
		granularity = "day"
	} else {
		granularity = "hour"
	}
	out.Series.Granularity = granularity
	rawFrom, dailyFrom := env.rawCut, env.dailyCut
	out.Coverage.RawFrom, out.Coverage.DailyFrom = &rawFrom, &dailyFrom
	e = env.db.View(func(tx *bolt.Tx) error {
		keyLabels := map[string]string(nil)
		if view == viewKey || view == viewKeyModel {
			keyLabels = readKeyLabels(tx)
		}
		var agg Aggregate
		var buckets map[time.Time]*Bucket
		var groups map[string]*Group
		if loc == time.UTC {
			agg, buckets, groups, e = collect(tx, env, q, granularity, view, keyLabels)
		} else {
			agg, buckets, groups, e = collectLocal(tx, env, q, granularity, view, keyLabels, loc)
		}
		if e != nil {
			return e
		}
		out.Summary = agg
		out.Series.Buckets = sortedBuckets(buckets)
		if view == viewRequests {
			items, more, e := scanRequestPage(tx, env, q, q.Page, q.PageSize)
			if e != nil {
				return e
			}
			out.Records, out.Pagination.HasMore = items, more
		} else {
			paged, more := pageGroups(groups, q.Page, q.PageSize)
			out.Groups, out.Pagination.HasMore = paged, more
		}
		out.Coverage.DetailedRequests = out.Summary.DetailedRequests
		out.Coverage.Requests = out.Summary.Requests
		out.Coverage.BackfillPending = tx.Bucket([]byte("meta")).Get([]byte(statsBackfillDoneKey)) == nil
		return nil
	})
	return out, e
}

// Facets returns bounded, stable metadata options. It reads long-lived daily
// dimensions plus raw-retention hourly dimensions and Keys (including deleted
// tombstones); it never scans capture bodies or credential files.
func (s *Store) Facets(q FacetQuery) (FacetPage, error) {
	if q.Field != facetModel && q.Field != facetKeyID && q.Field != facetProvider && q.Field != facetAuthIndex {
		return FacetPage{}, ErrInvalid
	}
	limit, ok := facetLimit(q.Limit)
	if !ok {
		return FacetPage{}, ErrInvalid
	}
	cursor := ""
	if q.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil {
			return FacetPage{}, ErrInvalid
		}
		cursor = string(raw)
	}
	env, release, e := s.queryEnv()
	if e != nil {
		return FacetPage{}, e
	}
	defer release()
	values := map[string]string{}
	e = env.db.View(func(tx *bolt.Tx) error {
		if q.Field == facetKeyID {
			for id, label := range readKeyLabels(tx) {
				values[id] = label
			}
		}
		collectFacet := func(d daily) error {
			var value string
			switch q.Field {
			case facetModel:
				value = d.Model
			case facetKeyID:
				value = d.KeyID
			case facetProvider:
				value = d.Provider
			case facetAuthIndex:
				value = d.AuthIndex
			}
			if value != "" {
				if _, ok := values[value]; !ok {
					values[value] = value
				}
			}
			return nil
		}
		if e := scanRollups(tx.Bucket([]byte("daily")), env.dailyCut, day(env.now).AddDate(0, 0, 1), collectFacet); e != nil {
			return e
		}
		return scanRollups(tx.Bucket([]byte("hourly")), env.rawCut.UTC().Truncate(time.Hour), env.now.Add(time.Hour), collectFacet)
	})
	if e != nil {
		return FacetPage{}, e
	}
	if q.Field == facetAuthIndex && len(q.Labels) > 0 {
		// Replace labels only for AuthIndex values that actually appeared in
		// retained records. Never inject a host account that Observer has no
		// history for, and keep value/cursor/sort identity on the index.
		for value := range values {
			if label := q.Labels[value]; label != "" {
				values[value] = label
			}
		}
	}
	search := strings.ToLower(strings.TrimSpace(q.Search))
	type item struct{ value, label string }
	all := make([]item, 0, len(values))
	for value, label := range values {
		if cursor != "" && value <= cursor {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(value), search) && !strings.Contains(strings.ToLower(label), search) {
			continue
		}
		all = append(all, item{value: value, label: label})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].value < all[j].value })
	out := FacetPage{Items: []FacetItem{}}
	end := limit
	if end > len(all) {
		end = len(all)
	}
	for _, v := range all[:end] {
		out.Items = append(out.Items, FacetItem{Value: v.value, Label: v.label})
	}
	if end < len(all) {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(all[end-1].value))
	}
	return out, nil
}

func facetLimit(limit int) (int, bool) {
	if limit < 0 {
		return 0, false
	}
	if limit == 0 {
		return 25, true
	}
	if limit > 100 {
		return 0, false
	}
	return limit, true
}

func readKeyLabels(tx *bolt.Tx) map[string]string {
	out := map[string]string{}
	b := tx.Bucket([]byte("keys"))
	if b == nil {
		return out
	}
	b.ForEach(func(k, v []byte) error {
		var stored storedKey
		if decode(v, &stored) == nil {
			label := stored.Label
			if label == "" {
				label = stored.ID
			}
			out[string(k)] = label
		}
		return nil
	})
	return out
}

func scanRequestPage(tx *bolt.Tx, env *queryEnv, q Query, page, size int) ([]Record, bool, error) {
	out := []Record{}
	skip := (page - 1) * size
	from := q.From
	if from.Before(env.rawCut) {
		from = env.rawCut
	}
	lower := timePrefix(from)
	c := tx.Bucket([]byte("timeline")).Cursor()
	k, v := c.Seek([]byte(timePrefix(q.To)))
	if k == nil {
		k, v = c.Last()
	} else {
		k, v = c.Prev()
	}
	for ; k != nil; k, v = c.Prev() {
		if string(k) < lower {
			break
		}
		var r Record
		if e := get(tx.Bucket([]byte("records")), string(v), &r); e != nil {
			return out, false, e
		}
		if r.Time.Before(env.rawCut) || r.Time.Before(q.From) {
			break
		}
		if !r.Time.Before(q.To) || !matches(q, r.KeyID, r.Model, r.Provider, r.AuthIndex, r.Failed) {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		if len(out) == size {
			return out, true, nil
		}
		out = append(out, r)
	}
	return out, false, nil
}

func summaryGranularity(env *queryEnv, q Query) (string, error) {
	cut := env.rawCut
	hourly := !q.From.Before(cut)
	if (q.From.Before(cut) && !q.From.Equal(day(q.From))) || (q.From.Before(cut) && day(q.To).Before(cut) && !q.To.Equal(day(q.To))) {
		return "", ErrInvalid
	}
	if hourly {
		return "hour", nil
	}
	return "day", nil
}

type groupAgg struct {
	key, label, model, provider, keyID, auth string
	agg                                      Aggregate
}

type aggregator struct {
	view      string
	daily     bool
	keyLabels map[string]string
	total     Aggregate
	series    map[time.Time]*Bucket
	groups    map[string]*Group
}

func newAggregator(view string, keyLabels map[string]string) *aggregator {
	return &aggregator{view: view, keyLabels: keyLabels, series: map[time.Time]*Bucket{}, groups: map[string]*Group{}}
}

func (a *aggregator) add(at time.Time, key, model, provider, auth string, failed bool, agg Aggregate) error {
	if a.daily {
		at = day(at)
	}
	if e := addAggregate(&a.total, agg); e != nil {
		return e
	}
	b := a.series[at]
	if b == nil {
		b = &Bucket{Time: at}
		a.series[at] = b
	}
	if e := addAggregate(&b.Aggregate, agg); e != nil {
		return e
	}
	if a.view == viewRequests {
		return nil
	}
	gk, label, gm, gp, kid, ga := groupIdentity(a.view, key, model, provider, auth, a.keyLabels)
	g := a.groups[gk]
	if g == nil {
		g = &Group{Key: gk, Label: label, Model: gm, Provider: gp, KeyID: kid, AuthIndex: ga}
		a.groups[gk] = g
	}
	return addAggregate(&g.Aggregate, agg)
}

func groupIdentity(view, key, model, provider, auth string, labels map[string]string) (gk, label, gm, gp, kid, ga string) {
	switch view {
	case viewModel:
		if model == "" && provider == "" {
			return "unknown", "unknown", "", "", "", ""
		}
		gk = captureID(provider, model)
		label = model
		if label == "" {
			label = "unknown"
		}
		return gk, label, model, provider, "", ""
	case viewKey:
		if key == "" {
			return "unknown", "unknown", "", "", "", ""
		}
		label = labels[key]
		if label == "" {
			label = key
		}
		return key, label, "", "", key, ""
	case viewKeyModel:
		if key == "" && model == "" && provider == "" {
			return "unknown", "unknown", "", "", "", ""
		}
		raw, _ := json.Marshal([]string{key, provider, model})
		label = labels[key]
		if label == "" {
			label = key
		}
		return string(raw), label, model, provider, key, ""
	case viewAccount:
		if auth == "" {
			return "unknown", "unknown", "", "", "", ""
		}
		return auth, auth, "", "", "", auth
	case viewProvider:
		if provider == "" {
			return "unknown", "unknown", "", "", "", ""
		}
		return provider, provider, "", provider, "", ""
	}
	return "unknown", "unknown", "", "", "", ""
}

func pageGroups(groups map[string]*Group, page, size int) ([]Group, bool) {
	all := make([]Group, 0, len(groups))
	for _, g := range groups {
		all = append(all, *g)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Requests != all[j].Requests {
			return all[i].Requests > all[j].Requests
		}
		return all[i].Key < all[j].Key
	})
	skip := (page - 1) * size
	if skip >= len(all) {
		return []Group{}, false
	}
	end := skip + size
	more := end < len(all)
	if end > len(all) {
		end = len(all)
	}
	return all[skip:end], more
}

func collect(tx *bolt.Tx, env *queryEnv, q Query, granularity, view string, keyLabels map[string]string) (Aggregate, map[time.Time]*Bucket, map[string]*Group, error) {
	a := newAggregator(view, keyLabels)
	a.daily = granularity == "day"
	if granularity == "hour" {
		if e := collectHourly(tx, env, q, a); e != nil {
			return Aggregate{}, nil, nil, e
		}
	} else {
		if e := collectDaily(tx, env, q, a); e != nil {
			return Aggregate{}, nil, nil, e
		}
	}
	return a.total, a.series, a.groups, nil
}

func collectHourly(tx *bolt.Tx, env *queryEnv, q Query, a *aggregator) error {
	first := q.From.UTC().Truncate(time.Hour)
	if first.Before(q.From) {
		first = first.Add(time.Hour)
	}
	last := q.To.UTC().Truncate(time.Hour)
	fullEnd := last
	if fullEnd.After(q.To) {
		fullEnd = q.To
	}
	if first.Before(fullEnd) {
		if e := scanRollups(tx.Bucket([]byte("hourly")), first, fullEnd, func(d daily) error {
			if !matches(q, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed) {
				return nil
			}
			return a.add(d.Bucket.Time, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed, d.Bucket.Aggregate)
		}); e != nil {
			return e
		}
	}
	// Only the two partial edge hours read raw records.
	if q.From.Before(first) {
		if e := scanRecordRange(tx, env, q, q.From, minTime(q.To, first), func(r Record) error {
			return a.add(r.Time.UTC().Truncate(time.Hour), r.KeyID, r.Model, r.Provider, r.AuthIndex, r.Failed, recordAggregate(r))
		}); e != nil {
			return e
		}
	}
	if !last.Before(q.From) && last.Before(q.To) {
		start := last
		if start.Before(q.From) {
			start = q.From
		}
		if e := scanRecordRange(tx, env, q, start, q.To, func(r Record) error {
			return a.add(r.Time.UTC().Truncate(time.Hour), r.KeyID, r.Model, r.Provider, r.AuthIndex, r.Failed, recordAggregate(r))
		}); e != nil {
			return e
		}
	}
	if !a.daily && q.To.Sub(q.From) <= 168*time.Hour {
		for t := q.From.UTC().Truncate(time.Hour); t.Before(q.To); t = t.Add(time.Hour) {
			if a.series[t] == nil {
				a.series[t] = &Bucket{Time: t}
			}
		}
	}
	return nil
}

func collectDaily(tx *bolt.Tx, env *queryEnv, q Query, a *aggregator) error {
	full := func(t time.Time) bool {
		return !t.Before(q.From) && !t.AddDate(0, 0, 1).After(q.To) && !t.Before(env.dailyCut)
	}
	start := day(q.From)
	if start.Before(env.dailyCut) {
		start = env.dailyCut
	}
	if e := scanRollups(tx.Bucket([]byte("daily")), start, day(q.To).AddDate(0, 0, 1), func(d daily) error {
		if full(d.Bucket.Time) && matches(q, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed) {
			return a.add(d.Bucket.Time, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed, d.Bucket.Aggregate)
		}
		return nil
	}); e != nil {
		return e
	}
	// Partial days reuse hourly rollups too. Decoding all surviving raw records
	// for the partial "today" tail makes a month view scale with raw traffic,
	// despite those hours already having transactional aggregates.
	from := q.From
	if from.Before(env.rawCut) {
		from = env.rawCut
	}
	for at := day(from); at.Before(q.To); at = at.AddDate(0, 0, 1) {
		if full(at) {
			continue
		}
		edge := q
		edge.From = from
		if at.After(edge.From) {
			edge.From = at
		}
		edge.To = minTime(q.To, at.AddDate(0, 0, 1))
		if e := collectHourly(tx, env, edge, a); e != nil {
			return e
		}
	}
	return nil
}

// collectLocal reads retained UTC minute aggregates once and projects them into
// local calendar days or monotonically elapsed local-hour buckets. It never
// mixes hourly/daily rollups or raw edges into the same range, so each minute
// contributes exactly once even when local and UTC dates differ.
func collectLocal(tx *bolt.Tx, env *queryEnv, q Query, granularity, view string, keyLabels map[string]string, loc *time.Location) (Aggregate, map[time.Time]*Bucket, map[string]*Group, error) {
	a := newAggregator(view, keyLabels)
	anchor := q.From.UTC()
	from := anchor
	if from.Before(env.dailyCut) {
		// Minute aggregates live for the daily retention window. Read-time
		// filtering keeps an overdue cleanup from reviving expired buckets.
		from = env.dailyCut
	}
	if e := scanRollups(tx.Bucket([]byte("minutes")), from, q.To, func(d daily) error {
		if d.Bucket.Time.Before(env.dailyCut) || !matches(q, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed) {
			return nil
		}
		at := localBucketStart(d.Bucket.Time, anchor, granularity, loc)
		return a.add(at, d.KeyID, d.Model, d.Provider, d.AuthIndex, d.Failed, d.Bucket.Aggregate)
	}); e != nil {
		return Aggregate{}, nil, nil, e
	}
	if granularity == "hour" && q.To.Sub(anchor) <= 168*time.Hour {
		// Elapsed-hour stepping retains both repeated instants of a fall-back
		// hour instead of collapsing equal wall-clock labels.
		for t := anchor; t.Before(q.To); t = t.Add(time.Hour) {
			if a.series[t] == nil {
				a.series[t] = &Bucket{Time: t}
			}
		}
	}
	return a.total, a.series, a.groups, nil
}

func localBucketStart(at, from time.Time, granularity string, loc *time.Location) time.Time {
	if granularity == "day" {
		return localDayStart(at, loc)
	}
	elapsed := at.Sub(from)
	if elapsed < 0 {
		elapsed = 0
	}
	return from.Add(elapsed / time.Hour * time.Hour).UTC()
}

// scanRecordRange walks the reverse timeline only for an explicit partial edge.
// It never decodes capture bodies or scans outside the requested interval.
func scanRecordRange(tx *bolt.Tx, env *queryEnv, q Query, from, to time.Time, fn func(Record) error) error {
	if !to.After(from) {
		return nil
	}
	if from.Before(env.rawCut) {
		from = env.rawCut
	}
	lower := timePrefix(from)
	c := tx.Bucket([]byte("timeline")).Cursor()
	k, v := c.Seek([]byte(timePrefix(to)))
	if k == nil {
		k, v = c.Last()
	} else {
		k, v = c.Prev()
	}
	for ; k != nil; k, v = c.Prev() {
		if string(k) < lower {
			break
		}
		var r Record
		if e := get(tx.Bucket([]byte("records")), string(v), &r); e != nil {
			return e
		}
		if r.Time.Before(from) || r.Time.Before(env.rawCut) {
			break
		}
		if !r.Time.Before(to) || !r.Time.Before(q.To) || !matches(q, r.KeyID, r.Model, r.Provider, r.AuthIndex, r.Failed) {
			continue
		}
		if e := fn(r); e != nil {
			return e
		}
	}
	return nil
}

func sortedBuckets(buckets map[time.Time]*Bucket) []Bucket {
	out := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func (e *queryEnv) normalizeView(v ViewQuery) (Query, string, string, *time.Location, error) {
	q := v.Query
	view := v.View
	if view == "" {
		view = viewRequests
	}
	if view != viewRequests && view != viewModel && view != viewKey && view != viewKeyModel && view != viewAccount && view != viewProvider {
		return Query{}, "", "", nil, ErrInvalid
	}
	loc, err := viewLocation(v.Timezone)
	if err != nil {
		return Query{}, "", "", nil, ErrInvalid
	}
	zone := "UTC"
	if loc.String() != "UTC" {
		zone = loc.String()
	}
	if v.FromDate == "" && v.ToDate == "" && !q.From.IsZero() && !q.To.IsZero() {
		// Direct callers may use the legacy RFC3339 half-open range.
	} else {
		nowLocal := e.now.In(loc)
		today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
		fromDate, toDate := v.FromDate, v.ToDate
		if fromDate == "" {
			fromDate = today.Format("2006-01-02")
		}
		if toDate == "" {
			toDate = today.Format("2006-01-02")
		}
		from, e1 := parseLocalDate(fromDate, loc)
		to, e2 := parseLocalDate(toDate, loc)
		if e1 != nil || e2 != nil || to.Before(from) {
			return Query{}, "", "", nil, ErrInvalid
		}
		// Calendar addition gives the correct local day-after instant across
		// 23/25-hour DST days; all response/storage instants are UTC.
		q.From = from.UTC()
		q.To = to.AddDate(0, 0, 1).UTC()
		if q.To.After(e.now) {
			q.To = e.now
		}
	}
	page, size, ok := pageLimits(q.Page, q.PageSize, 100)
	if !ok {
		return Query{}, "", "", nil, ErrInvalid
	}
	q.Page, q.PageSize = page, size
	if !validQuery(q) {
		return Query{}, "", "", nil, ErrInvalid
	}
	return q, view, zone, loc, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func timePrefix(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func timeID(r Record) string        { return timePrefix(r.Time) + r.ID }
