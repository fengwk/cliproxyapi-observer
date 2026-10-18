package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ci runs the canonical Go and Node commands over every package. The three
// Go gates must stay explicit so a CI edit cannot silently downgrade unit,
// race or vet coverage. The rebuilt frontend uses behavioral Vitest tests and
// a reproducible embedded build instead of literal-source legacy JS tests.
func TestCIWorkflowRunsFullGoAndNodeSuites(t *testing.T) {
	doc, raw := loadWorkflow(t, ".github/workflows/ci.yml")

	testJob, ok := doc.Jobs["test"]
	if !ok {
		t.Fatal("ci must define the test job")
	}
	for name, command := range map[string]string{
		"Vet":                    "go vet ./...",
		"Go tests":               "go test ./...",
		"Go race tests":          "go test -race ./...",
		"Install frontend tools": "npm ci --prefix web",
		"Audit frontend tools":   "npm audit --prefix web",
		"Frontend typecheck":     "npm --prefix web run typecheck",
		"Frontend tests":         "npm --prefix web test",
	} {
		step, ok := stepByName(testJob.Steps, name)
		if !ok || strings.TrimSpace(step.Run) != command {
			t.Fatalf("%s must run %q, got %q", name, command, step.Run)
		}
	}
	node, ok := stepByName(testJob.Steps, "Node tests")
	if !ok || !strings.Contains(node.Run, "node --test") {
		t.Fatal("ci must run the Node mock API contract tests")
	}
	for _, file := range []string{
		"internal/web/browser/mock-server.test.cjs",
	} {
		if !strings.Contains(node.Run, file) {
			t.Fatalf("Node tests must include %s, got %q", file, node.Run)
		}
	}
	build, ok := stepByName(testJob.Steps, "Verify committed frontend build")
	if !ok || !strings.Contains(build.Run, "npm --prefix web run build") ||
		!strings.Contains(build.Run, "git diff --exit-code -- internal/web/dist") {
		t.Fatal("ci must verify source matches committed embedded assets")
	}
	if strings.Contains(raw, "go test -short") || strings.Contains(raw, "-run ") {
		t.Fatal("ci must not narrow the Go package or test selection")
	}
}

// Browser acceptance uses Playwright and the official CPA shell against the
// Chrome image installed on the Ubuntu runner. It must verify the binary,
// execute the documented make target and always retain the report and
// screenshots for diagnosis.
func TestCIBrowserRunsRealChromeAndUploadsEvidence(t *testing.T) {
	doc, raw := loadWorkflow(t, ".github/workflows/ci.yml")

	job, ok := doc.Jobs["browser"]
	if !ok {
		t.Fatal("ci must define a real Chrome browser job")
	}
	if job.RunsOn != "ubuntu-latest" {
		t.Fatalf("browser acceptance must run on ubuntu-latest, got %q", job.RunsOn)
	}
	if job.Env["CHROME_BIN"] != "/usr/bin/google-chrome-stable" {
		t.Fatalf("browser job must bind CHROME_BIN to the installed Ubuntu Chrome, got %q",
			job.Env["CHROME_BIN"])
	}
	if !contains(needsList(job.Needs), "test") {
		t.Fatalf("browser acceptance must depend on the canonical test job, got %v", job.Needs)
	}
	install, ok := stepByName(job.Steps, "Install frontend tools")
	if !ok || strings.TrimSpace(install.Run) != "npm ci --prefix web" {
		t.Fatal("browser job must install the locked Playwright build tools")
	}

	verify, ok := stepByName(job.Steps, "Verify installed Chrome")
	if !ok || !strings.Contains(verify.Run, "test -x \"$CHROME_BIN\"") ||
		!strings.Contains(verify.Run, "--version") {
		t.Fatal("browser acceptance must verify the installed Chrome binary before running")
	}
	acceptance, ok := stepByName(job.Steps, "Browser acceptance")
	if !ok || strings.TrimSpace(acceptance.Run) != "make browser" {
		t.Fatalf("browser acceptance must use the documented make target, got %q", acceptance.Run)
	}
	if !strings.Contains(acceptance.Env["BROWSER_REPORT_DIR"], "runner.temp") {
		t.Fatalf("browser report dir must stay outside the worktree, got %q",
			acceptance.Env["BROWSER_REPORT_DIR"])
	}

	upload, ok := stepByName(job.Steps, "Upload browser report and screenshots")
	if !ok || !strings.Contains(upload.If, "always()") ||
		!strings.HasPrefix(upload.Uses, "actions/upload-artifact@") {
		t.Fatal("browser evidence must be uploaded even when acceptance fails")
	}
	paths := fmt.Sprint(upload.With["path"])
	for _, want := range []string{"report.json", "summary.txt", "screenshots/"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("browser artifact must include %s, got %q", want, paths)
		}
	}
	if strings.Contains(raw, "secrets.") {
		t.Fatal("browser acceptance must not reference repository secrets")
	}
}

// v0.6.0 drops the retired v8.0.15 host. CI validates the pinned SDK baseline
// host and the newest stable v8 release.
func TestCIRealHostMatrixCoversBaselineAndLatest(t *testing.T) {
	doc, _ := loadWorkflow(t, ".github/workflows/ci.yml")

	job, ok := doc.Jobs["real-host"]
	if !ok {
		t.Fatal("ci must keep the real-host job")
	}
	if job.Uses != "./.github/workflows/real-host.yml" {
		t.Fatalf("real-host must use the reusable workflow, got %q", job.Uses)
	}
	tags := job.Strategy.Matrix.CpaTag
	if len(tags) != 2 || !contains(tags, "v8.0.30") || !contains(tags, "latest") {
		t.Fatalf("real-host matrix must be exactly v8.0.30 and latest, got %v", tags)
	}
	if contains(tags, "v8.0.15") {
		t.Fatal("the retired v8.0.15 host must no longer run in CI")
	}
}

// The reusable real-host lane must finish inside ten minutes while the Go
// integration suite itself has a 300s timeout.
func TestRealHostIntegrationTimeout(t *testing.T) {
	doc, raw := loadWorkflow(t, ".github/workflows/real-host.yml")

	job, ok := doc.Jobs["real-host"]
	if !ok {
		t.Fatal("real-host must define the real-host job")
	}
	if job.Timeout != 10 {
		t.Fatalf("real-host job must time out after 10 minutes, got %d", job.Timeout)
	}
	run, ok := stepByName(job.Steps, "Run real-host integration suite")
	if !ok || strings.TrimSpace(run.Run) != "make integration" {
		t.Fatalf("real-host must run the canonical integration target, got %q", run.Run)
	}
	if strings.TrimSpace(run.Env["CPA_BINARY"]) == "" {
		t.Fatal("real-host must provide CPA_BINARY to the integration suite")
	}
	if !strings.Contains(raw, "timeout-minutes: 10") {
		t.Fatal("real-host workflow must declare a ten-minute job timeout")
	}
}

// Native browser acceptance must not silently become an optional skip in CI.
// The reusable lane covers both baseline/latest and archives real host evidence.
func TestRealHostBrowserIsRequiredAndArchived(t *testing.T) {
	doc, _ := loadWorkflow(t, ".github/workflows/real-host.yml")
	job := doc.Jobs["real-host"]
	var node22 bool
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/setup-node@") && fmt.Sprint(step.With["node-version"]) == "22" {
			node22 = true
		}
	}
	if !node22 || job.Env["CHROME_BIN"] != "/usr/bin/google-chrome-stable" {
		t.Fatal("real-host browser requires Node 22 and installed Chrome")
	}
	install, ok := stepByName(job.Steps, "Install frontend tools")
	if !ok || strings.TrimSpace(install.Run) != "npm ci --prefix web" {
		t.Fatal("real-host browser must install the locked Playwright build tools")
	}
	verify, ok := stepByName(job.Steps, "Verify installed Chrome")
	if !ok || !strings.Contains(verify.Run, "test -x \"$CHROME_BIN\"") {
		t.Fatal("real-host must verify Chrome before testing")
	}
	run, ok := stepByName(job.Steps, "Run real-host integration suite")
	if !ok || run.Env["OBSERVER_BROWSER"] != "1" ||
		!strings.Contains(run.Env["CPA_TEST_ARTIFACTS"], "runner.temp") {
		t.Fatal("real-host CI must enable native browser and retain artifacts")
	}
	upload, ok := stepByName(job.Steps, "Upload native host and browser evidence")
	if !ok || !strings.Contains(upload.If, "always()") ||
		!strings.HasPrefix(upload.Uses, "actions/upload-artifact@") ||
		!strings.Contains(fmt.Sprint(upload.With["path"]), "native-host-report") {
		t.Fatal("real-host evidence must always be archived")
	}
}

// The Makefile is the canonical local interface: frontend behavior and the mock
// API are required, browser acceptance invokes the documented Chrome runner, and
// integration requires a real host binary with a 300s Go test timeout.
func TestMakefileCanonicalTargets(t *testing.T) {
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(data)

	for _, want := range []string{
		"go vet ./...",
		"go test ./...",
		"go test -race ./...",
		"npm --prefix web run typecheck",
		"npm --prefix web test",
		"npm --prefix web run build",
		"internal/web/browser/mock-server.test.cjs",
		"OBSERVER_BROWSER ?= internal/web/browser/run.cjs",
		"BROWSER_REPORT_DIR ?=",
		"CHROME_BIN ?=",
		"node \"$(OBSERVER_BROWSER)\" --report-dir \"$(BROWSER_REPORT_DIR)\"",
		"CPA_BINARY",
		"-timeout=300s",
	} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("Makefile must contain %q", want)
		}
	}
	if strings.Contains(makefile, "-timeout=120s") {
		t.Fatal("integration timeout must be raised to 300s")
	}
	if !strings.Contains(makefile, "\nbrowser:\n") {
		t.Fatal("Makefile must expose a browser target")
	}
}
