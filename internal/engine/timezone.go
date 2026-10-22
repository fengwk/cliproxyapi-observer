package engine

import (
	"strings"
	"time"
	_ "time/tzdata" // keep named zones available in the native plugin binary
)

// viewLocation resolves the request timezone. An omitted timezone means UTC;
// "Local" and unknown names are rejected so results never depend on the host's
// OS configuration.
func viewLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if name == "Local" || strings.TrimSpace(name) != name {
		return nil, ErrInvalid
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return nil, ErrInvalid
	}
	return loc, nil
}

type civilDate struct {
	year  int
	month time.Month
	day   int
}

func civilOf(t time.Time) civilDate {
	y, m, d := t.Date()
	return civilDate{year: y, month: m, day: d}
}

func (d civilDate) compare(other civilDate) int {
	switch {
	case d.year != other.year:
		if d.year < other.year {
			return -1
		}
		return 1
	case d.month != other.month:
		if d.month < other.month {
			return -1
		}
		return 1
	case d.day != other.day:
		if d.day < other.day {
			return -1
		}
		return 1
	default:
		return 0
	}
}

// parseLocalDate parses an exact YYYY-MM-DD calendar date and returns its first
// representable instant in loc. Midnight may be skipped by a DST transition;
// such a date is still valid and starts at the first later instant (for example
// 01:00). A date skipped in its entirety is rejected.
func parseLocalDate(value string, loc *time.Location) (time.Time, error) {
	if len(value) != len("2006-01-02") {
		return time.Time{}, ErrInvalid
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil || t.Format("2006-01-02") != value {
		return time.Time{}, ErrInvalid
	}
	y, m, d := t.Date()
	start, ok := firstLocalInstant(civilDate{year: y, month: m, day: d}, loc)
	if !ok {
		return time.Time{}, ErrInvalid
	}
	return start, nil
}

// localDayEnd returns the first representable instant after the local calendar
// date containing day. It is based on the next date's own first instant rather
// than carrying a normalized wall clock into the next day.
func localDayEnd(day time.Time, loc *time.Location) (time.Time, error) {
	target := civilOf(day.In(loc))
	noon := time.Date(target.year, target.month, target.day, 12, 0, 0, 0, loc)
	end, ok := firstInstantAfter(noon, noon.Add(48*time.Hour), target, loc)
	if !ok {
		return time.Time{}, ErrInvalid
	}
	return end.UTC(), nil
}

// localDayStart returns the UTC instant at which t's local calendar date
// begins. For dates whose midnight is skipped this is the first later instant.
func localDayStart(t time.Time, loc *time.Location) time.Time {
	start, ok := firstLocalInstant(civilOf(t.In(loc)), loc)
	if !ok {
		// Aggregated rows always belong to a representable local date.
		return t.In(loc).UTC()
	}
	return start.UTC()
}

func firstLocalInstant(target civilDate, loc *time.Location) (time.Time, bool) {
	// Ordinary midnights are already exact. Only transitions that skip or
	// repeat midnight need the search, not every retained minute in a chart.
	midnight := time.Date(target.year, target.month, target.day, 0, 0, 0, 0, loc)
	if civilOf(midnight) == target && midnight.Hour() == 0 &&
		civilOf(midnight.Add(-time.Second).In(loc)).compare(target) < 0 {
		return midnight, true
	}
	noon := time.Date(target.year, target.month, target.day, 12, 0, 0, 0, loc)
	first, ok := firstInstantAtOrAfter(noon.Add(-48*time.Hour), noon, target, loc)
	if !ok || civilOf(first.In(loc)).compare(target) != 0 {
		return time.Time{}, false
	}
	return first, true
}

// firstInstantAtOrAfter finds the first whole second whose local calendar date
// is at or after target. IANA transitions occur on whole seconds, so this is
// exact for the supported retention window.
func firstInstantAtOrAfter(lower, upper time.Time, target civilDate, loc *time.Location) (time.Time, bool) {
	lo, hi := lower.Unix(), upper.Unix()
	if civilOf(time.Unix(hi, 0).In(loc)).compare(target) < 0 {
		return time.Time{}, false
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if civilOf(time.Unix(mid, 0).In(loc)).compare(target) >= 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return time.Unix(lo, 0).In(loc), true
}

func firstInstantAfter(lower, upper time.Time, target civilDate, loc *time.Location) (time.Time, bool) {
	lo, hi := lower.Unix(), upper.Unix()
	if civilOf(time.Unix(hi, 0).In(loc)).compare(target) <= 0 {
		return time.Time{}, false
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if civilOf(time.Unix(mid, 0).In(loc)).compare(target) > 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return time.Unix(lo, 0).In(loc), true
}
