package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowStep is the subset of a workflow step the structural tests inspect.
type workflowStep struct {
	Name string                 `yaml:"name"`
	Uses string                 `yaml:"uses"`
	With map[string]interface{} `yaml:"with"`
	Env  map[string]string      `yaml:"env"`
	Run  string                 `yaml:"run"`
	If   string                 `yaml:"if"`
}

// workflowDoc is the subset of a workflow file the structural tests inspect.
type workflowDoc struct {
	Name        string                 `yaml:"name"`
	RunName     string                 `yaml:"run-name"`
	On          map[string]interface{} `yaml:"on"`
	Env         map[string]string      `yaml:"env"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency *struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If          string                 `yaml:"if"`
		Needs       interface{}            `yaml:"needs"`
		Uses        string                 `yaml:"uses"`
		With        map[string]interface{} `yaml:"with"`
		Permissions map[string]string      `yaml:"permissions"`
		Steps       []workflowStep         `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, path string) (workflowDoc, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc workflowDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, string(data)
}

func stepByName(steps []workflowStep, name string) (workflowStep, bool) {
	for _, step := range steps {
		if step.Name == name {
			return step, true
		}
	}
	return workflowStep{}, false
}

// A retry must publish the original tag, not rebuild the current default branch,
// and the publisher helper must come from the trusted default branch so an old
// tag never executes its own helper code. Runs for one tag must be serialized.
func TestReleaseRetryPreservesTagAndRepository(t *testing.T) {
	doc, _ := loadWorkflow(t, ".github/workflows/release.yml")
	if _, ok := doc.On["workflow_dispatch"]; !ok {
		t.Fatal("release workflow must allow retrying an existing tag")
	}
	if doc.Env["RELEASE_TAG"] != "${{ inputs.tag || github.ref_name }}" {
		t.Fatal("tag push and manual retry must select the same release tag")
	}
	if doc.Concurrency == nil || doc.Concurrency.CancelInProgress ||
		!strings.Contains(doc.Concurrency.Group, "inputs.tag") ||
		!strings.Contains(doc.Concurrency.Group, "github.ref_name") {
		t.Fatal("release runs for one tag must share a non-cancelling concurrency group")
	}
	build := doc.Jobs["build"].Steps
	if len(build) == 0 || !strings.HasPrefix(build[0].Uses, "actions/checkout@") ||
		fmt.Sprint(build[0].With["ref"]) != "refs/tags/${{ env.RELEASE_TAG }}" {
		t.Fatal("release builds must check out the exact selected tag")
	}
	var commitResolved, commitPackaged bool
	if step, ok := stepByName(build, "Resolve plugin version"); ok {
		commitResolved = step.Env["PLUGIN_VERSION"] == "${{ env.RELEASE_TAG }}" &&
			strings.Contains(step.Run, "git rev-parse HEAD")
	}
	if step, ok := stepByName(build, "Package archive"); ok {
		commitPackaged = step.Env["COMMIT"] == "${{ steps.version.outputs.commit }}"
	}

	var helperFromDefaultBranch, repositoryBound bool
	for _, step := range doc.Jobs["publish"].Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			helperFromDefaultBranch = fmt.Sprint(step.With["ref"]) ==
				"${{ github.event.repository.default_branch }}"
		}
		if step.Name == "Publish GitHub Release" {
			repositoryBound = step.Env["GH_REPO"] == "${{ github.repository }}" &&
				step.Env["TAG"] == "${{ env.RELEASE_TAG }}" &&
				strings.Contains(step.Run, "scripts/publish_release.py")
		}
	}
	if !commitResolved || !commitPackaged || !helperFromDefaultBranch || !repositoryBound {
		t.Fatalf("release identity lost: resolved=%v packaged=%v helper=%v repository=%v",
			commitResolved, commitPackaged, helperFromDefaultBranch, repositoryBound)
	}
}

// The run display title must carry the release tag so the auto-release
// controller can find the matching workflow_dispatch run, whose head branch
// (main) differs from the tag.
func TestReleaseWorkflowExposesTagInRunName(t *testing.T) {
	doc, _ := loadWorkflow(t, ".github/workflows/release.yml")
	if !strings.Contains(doc.RunName, "inputs.tag") || !strings.Contains(doc.RunName, "github.ref_name") {
		t.Fatalf("release run-name must include the selected tag, got %q", doc.RunName)
	}
}

// The publisher owns the gh release calls; it must create a draft with
// --verify-tag and never clobber assets or delete tags.
func TestPublishHelperNeverClobbers(t *testing.T) {
	data, err := os.ReadFile("scripts/publish_release.py")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "--verify-tag") || !strings.Contains(src, "--draft") {
		t.Fatal("publisher must create the release as a draft bound to the existing tag")
	}
	if strings.Contains(src, "--clobber") || strings.Contains(src, "--cleanup-tag") {
		t.Fatal("publisher must never clobber assets or delete tags")
	}
}
