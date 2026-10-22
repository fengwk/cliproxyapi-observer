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

// parseLocalDate parses an exact YYYY-MM-DD calendar date in loc. The round
// trip rejects dates that the parser would otherwise normalize into another
// calendar day (for example a wholly skipped local date).
func parseLocalDate(value string, loc *time.Location) (time.Time, error) {
	if len(value) != len("2006-01-02") {
		return time.Time{}, ErrInvalid
	}
	t, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil || t.Format("2006-01-02") != value {
		return time.Time{}, ErrInvalid
	}
	return t, nil
}

// localDayStart returns the UTC instant of the first representable local
// midnight for t's local calendar date. Calendar addition, not fixed 24-hour
// arithmetic, controls adjacent local days.
func localDayStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc).UTC()
}
