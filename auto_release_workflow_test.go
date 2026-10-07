package main

import (
	"fmt"
	"strings"
	"testing"
)

func stringList(value interface{}) []string {
	list, ok := value.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// The auto-release controller must run safely unattended: only on completed
// successful ci runs on the trusted default branch, on a schedule and manually,
// with least-privilege permissions, a non-cancelling shared concurrency group
// and a full-history checkout of the default branch.
func TestAutoReleaseWorkflowStructure(t *testing.T) {
	doc, raw := loadWorkflow(t, ".github/workflows/auto-release.yml")

	if _, ok := doc.On["workflow_dispatch"]; !ok {
		t.Fatal("auto-release must support a manual dispatch")
	}
	runTrigger, ok := doc.On["workflow_run"].(map[string]interface{})
	if !ok {
		t.Fatal("auto-release must trigger on workflow_run")
	}
	if workflows := stringList(runTrigger["workflows"]); !contains(workflows, "ci") {
		t.Fatalf("auto-release must watch the ci workflow, got %v", workflows)
	}
	if types := stringList(runTrigger["types"]); !contains(types, "completed") {
		t.Fatalf("auto-release must run on completed ci runs, got %v", types)
	}

	schedules, _ := doc.On["schedule"].([]interface{})
	var cronFound bool
	for _, entry := range schedules {
		if spec, ok := entry.(map[string]interface{}); ok {
			cronFound = cronFound || fmt.Sprint(spec["cron"]) == "0 */6 * * *"
		}
	}
	if !cronFound {
		t.Fatal("auto-release must reconcile every six hours")
	}

	if doc.Permissions["contents"] != "read" {
		t.Fatal("the workflow default permission must be contents: read")
	}
	if doc.Concurrency == nil || doc.Concurrency.CancelInProgress {
		t.Fatal("auto-release must use a shared, non-cancelling concurrency group")
	}
	if strings.TrimSpace(doc.Concurrency.Group) == "" {
		t.Fatal("auto-release concurrency group must be set")
	}

	job, ok := doc.Jobs["reconcile"]
	if !ok {
		t.Fatal("auto-release must define the reconcile job")
	}
	for _, permission := range []string{"contents", "actions", "issues"} {
		if job.Permissions[permission] != "write" {
			t.Fatalf("reconcile job must grant %s: write, got %q", permission, job.Permissions[permission])
		}
	}
	if !strings.Contains(job.If, "conclusion == 'success'") ||
		!strings.Contains(job.If, "head_repository.full_name") {
		t.Fatal("reconcile job must gate workflow_run events on a successful same-repo ci run")
	}

	var checkedOut bool
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkedOut = fmt.Sprint(step.With["ref"]) == "${{ github.event.repository.default_branch }}" &&
				fmt.Sprint(step.With["fetch-depth"]) == "0"
		}
	}
	if !checkedOut {
		t.Fatal("reconcile must check out the full history of the trusted default branch")
	}

	controller, ok := stepByName(job.Steps, "Reconcile release")
	if !ok || !strings.Contains(controller.Run, "scripts/auto_release.py") ||
		controller.Env["GH_REPO"] != "${{ github.repository }}" {
		t.Fatal("reconcile must run the controller with an explicit repository")
	}
	if controller.Env["CI_RUN_ID"] != "${{ github.event.workflow_run.id }}" ||
		controller.Env["EXPECTED_HEAD_SHA"] != "${{ github.event.workflow_run.head_sha }}" {
		t.Fatal("reconcile must bind event-driven runs to the immutable ci run")
	}

	failure, ok := stepByName(job.Steps, "Report failure")
	if !ok || !strings.Contains(failure.If, "failure()") ||
		!strings.Contains(failure.Run, "gh issue create") ||
		!strings.Contains(failure.Env["RUN_URL"], "github.run_id") {
		t.Fatal("failures must be reported in one issue carrying only the run URL")
	}
	recovered, ok := stepByName(job.Steps, "Close recovered issue")
	if !ok || !strings.Contains(recovered.If, "success()") ||
		!strings.Contains(recovered.Run, "gh issue close") {
		t.Fatal("a healthy reconciliation must close the owned issue")
	}

	// No new secret or PAT may be introduced.
	if strings.Contains(raw, "secrets.") {
		t.Fatal("auto-release must not reference repository secrets")
	}
}
