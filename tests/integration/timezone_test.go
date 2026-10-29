//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// A real host request must turn browser-local inclusive dates into exact UTC
// half-open bounds and expose the effective IANA zone to the management UI.
func TestRequestViewTimezoneBoundsThroughRealHost(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	record := acceptanceInferAndAwait(t, h, created.Secret, map[string]struct{}{})

	loc, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	local := record.Time.In(loc)
	localDate := local.Format("2006-01-02")
	from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC()

	var view struct {
		Summary acceptanceSummary `json:"summary"`
		Range   struct {
			From     time.Time `json:"from"`
			To       time.Time `json:"to"`
			Timezone string    `json:"timezone"`
		} `json:"range"`
		Records []recordView `json:"records"`
	}
	path := "/requests/view?from_date=" + localDate + "&to_date=" + localDate +
		"&timezone=" + url.QueryEscape("Asia/Kathmandu") + "&view=requests"
	h.api(t, http.MethodGet, path, nil, http.StatusOK, &view)
	if view.Range.Timezone != "Asia/Kathmandu" || !view.Range.From.Equal(from) {
		t.Fatalf("local range = %+v, want timezone=Asia/Kathmandu from=%v", view.Range, from)
	}
	if view.Range.To.After(time.Now().Add(2*time.Second)) || !view.Range.To.After(view.Range.From) {
		t.Fatalf("local range end = %v, from=%v", view.Range.To, view.Range.From)
	}
	if view.Summary.Requests != 1 || len(view.Records) != 1 || view.Records[0].ID != record.ID {
		t.Fatalf("local-day request view = %+v/%+v", view.Summary, view.Records)
	}

	h.api(t, http.MethodGet, "/requests/view?from_date="+localDate+"&to_date="+localDate+"&timezone=Local", nil, http.StatusBadRequest, nil)
}
