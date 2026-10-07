package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// needsList normalizes a job's `needs` value, which YAML may parse as a scalar
// or a sequence.
func needsList(value interface{}) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return nil
	}
}

// The daily compatibility workflow must keep its reusable real-host check as
// the source of truth while adding unattended, deduplicated failure reporting:
// it tracks the reusable job result in a separate always() job, reports only
// the run URL, and never swallows the failed check.
func TestCompatibilityWorkflowReportsBreakage(t *testing.T) {
	doc, raw := loadWorkflow(t, ".github/workflows/compatibility.yml")

	// Scheduled and manual runs both exercise the newest stable v8 host.
	schedules, _ := doc.On["schedule"].([]interface{})
	var cronFound bool
	for _, entry := range schedules {
		if spec, ok := entry.(map[string]interface{}); ok {
			cronFound = cronFound || fmt.Sprint(spec["cron"]) == "23 6 * * *"
		}
	}
	if !cronFound {
		t.Fatal("compatibility must run on the daily schedule")
	}
	if _, ok := doc.On["workflow_dispatch"]; !ok {
		t.Fatal("compatibility must support a manual dispatch")
	}
	if doc.Permissions["contents"] != "read" {
		t.Fatal("the workflow default permission must stay contents: read")
	}

	check, ok := doc.Jobs["real-host-latest"]
	if !ok {
		t.Fatal("compatibility must keep the real-host-latest job")
	}
	if check.Uses != "./.github/workflows/real-host.yml" {
		t.Fatalf("compatibility must call the reusable real-host workflow, got %q", check.Uses)
	}
	if fmt.Sprint(check.With["cpa-tag"]) != "latest" {
		t.Fatalf("compatibility must test the newest stable v8 host, got %v", check.With["cpa-tag"])
	}

	report, ok := doc.Jobs["report"]
	if !ok {
		t.Fatal("compatibility must define a reporting job")
	}
	if strings.TrimSpace(report.Uses) != "" {
		t.Fatal("the reporting job must be an ordinary runner job, not a reusable call")
	}
	// A reusable workflow job cannot carry extra steps, so reporting runs in a
	// dependent job that must observe the reusable result rather than ignore it.
	if !contains(needsList(report.Needs), "real-host-latest") {
		t.Fatalf("reporting must depend on the reusable check, got %v", report.Needs)
	}
	if !strings.Contains(report.If, "always()") {
		t.Fatal("reporting must run regardless of the reusable check result")
	}
	if report.Permissions["contents"] != "read" || report.Permissions["issues"] != "write" {
		t.Fatalf("reporting needs contents: read + issues: write, got %v", report.Permissions)
	}

	var trustedCheckout, recorded bool
	for _, step := range report.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			trustedCheckout = fmt.Sprint(step.With["ref"]) ==
				"${{ github.event.repository.default_branch }}"
		}
		if step.Name == "Record compatibility outcome" {
			recorded = step.Env["GH_TOKEN"] == "${{ github.token }}" &&
				step.Env["GH_REPO"] == "${{ github.repository }}" &&
				step.Env["REPORT_RESULT"] == "${{ needs.real-host-latest.result }}" &&
				strings.Contains(step.Env["RUN_URL"], "github.run_id") &&
				strings.Contains(step.Env["RUN_URL"], "github.repository") &&
				strings.Contains(step.Run, "scripts/compatibility-report.sh")
		}
	}
	if !trustedCheckout || !recorded {
		t.Fatalf("reporting wiring is incomplete: trusted-checkout=%v recorded=%v", trustedCheckout, recorded)
	}

	// The failed reusable job must keep the run red: no step may swallow it, and
	// no secret or PAT may be introduced.
	if strings.Contains(raw, "continue-on-error") {
		t.Fatal("compatibility reporting must not swallow the failed check")
	}
	if strings.Contains(raw, "secrets.") {
		t.Fatal("compatibility reporting must not reference repository secrets")
	}
}

// The reporting helper must publish nothing but the run URL and must only touch
// its own bot-owned issue.
func TestCompatibilityReportPublishesOnlyRunURL(t *testing.T) {
	data, err := os.ReadFile("scripts/compatibility-report.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, forbidden := range []string{"run view", "--log", "GH_TOKEN", "secrets."} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("reporting must never publish logs or credentials (found %q)", forbidden)
		}
	}
	if !strings.Contains(src, "github-actions[bot]") || !strings.Contains(src, "--author") {
		t.Fatal("reporting must only comment on and close its own bot-owned issue")
	}
}
