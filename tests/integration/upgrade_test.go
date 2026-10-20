//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const upgradeBaseline = "37a2468f055c698158e50cde9b1758a8417e487e"

// Build the immutable prior release and create an actual nonempty schema2 DB
// with its own writer. The candidate must preserve credentials, fixed costs,
// old capture envelopes and detailed backfill across two real-host restarts.
func TestUpgradeFromActualV070PreservesLedgerAndCredentials(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	source, build := t.TempDir(), t.TempDir()
	archive := filepath.Join(t.TempDir(), "baseline.tar")
	for _, args := range [][]string{
		{"git", "-C", root, "archive", "--format=tar", "--output=" + archive, upgradeBaseline},
		{"tar", "-xf", archive, "-C", source},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("export pinned baseline (checkout must include its history): %v: %s", err, out)
		}
	}
	if err := buildPlugin(source, build); err != nil {
		t.Fatal(err)
	}
	// These tests are deliberately serial. Restore the shared candidate path
	// immediately after copying the old library into this isolated host.
	h := func() *harness {
		candidate := pluginBuildRoot
		pluginBuildRoot = build
		defer func() { pluginBuildRoot = candidate }()
		return newHarness(t)
	}()
	created := h.createKey(t, validSpec())
	settings := acceptanceGetSettings(t, h)
	if _, present := settings["capture_mode"]; present {
		t.Fatal("baseline is not the old writer")
	}
	settings["capture_enabled"] = true
	settings["pricing"] = acceptancePricing{Auto: false, Prices: []acceptancePrice{
		{Model: upstreamModel, Rates: acceptanceRatesAll(1_000_000), Source: "manual"},
	}, Rules: []acceptanceRule{}}
	h.api(t, http.MethodPut, "/settings", settings, 200, nil)
	known := map[string]struct{}{}
	before := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, before, 15, "manual")
	h.awaitGracefulRestartReady(t, 1)
	path := "/requests/detail?id=" + url.QueryEscape(before.ID)
	var oldDetail struct {
		CaptureStatus string `json:"capture_status"`
		Body          any    `json:"body"`
	}
	h.await(t, "old writer has durable content", func() bool {
		h.api(t, http.MethodGet, path, nil, 200, &oldDetail)
		return oldDetail.Body != nil
	})
	beforeKey := h.keys(t)[0]
	h.Stop()
	if err := h.waitError(); err != nil {
		t.Fatal("old host did not stop gracefully", err)
	}
	if err := installPlugin(filepath.Join(h.dir, "plugins")); err != nil {
		t.Fatal(err)
	}
	h, err = h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	afterKey := h.keys(t)[0]
	if afterKey.ID != beforeKey.ID || afterKey.Used != beforeKey.Used || afterKey.BlockedReason != "" {
		t.Fatal("upgrade changed key identity/accounting", beforeKey, afterKey)
	}
	if got := acceptanceRecordByID(t, h, before.ID); got != before {
		t.Fatal("upgrade changed the saved record", before, got)
	}
	var upgraded struct {
		Body          any    `json:"body"`
		HeadersStatus string `json:"headers_status"`
		BodyStatus    string `json:"body_status"`
	}
	h.api(t, http.MethodGet, path, nil, 200, &upgraded)
	if !reflect.DeepEqual(oldDetail.Body, upgraded.Body) || upgraded.HeadersStatus == "" || upgraded.BodyStatus == "" {
		t.Fatal("legacy content envelope not preserved", oldDetail, upgraded)
	}
	var view struct {
		Summary struct {
			Requests int64 `json:"requests"`
			Cost     int64 `json:"cost_micro_usd"`
			Detailed int64 `json:"detailed_requests"`
		} `json:"summary"`
	}
	date := time.Now().UTC().Format("2006-01-02")
	route := "/requests/view?from_date=" + date + "&to_date=" + date
	h.api(t, http.MethodGet, route, nil, 200, &view)
	if view.Summary.Requests != 1 || view.Summary.Cost != 15 || view.Summary.Detailed != 1 {
		t.Fatal("upgrade backfill duplicated totals or missed surviving detail", view)
	}
	acceptancePutPricing(t, h, acceptancePricing{Auto: false, Prices: []acceptancePrice{
		{Model: before.Model, Rates: acceptanceRatesAll(2_000_000), Source: "manual"},
	}, Rules: []acceptanceRule{}})
	if got := acceptanceRecordByID(t, h, before.ID); got != before {
		t.Fatal("price edit repriced history")
	}
	next := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, next, 30, "manual") // the exact old secret still authenticates
	h.awaitGracefulRestartReady(t, 2)
	h, err = h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	h.api(t, http.MethodGet, route, nil, 200, &view)
	if view.Summary.Requests != 2 || view.Summary.Cost != 45 || view.Summary.Detailed != 2 {
		t.Fatal("restart repeated the additive backfill", view)
	}
}
