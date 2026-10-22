//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const upgradeBaseline = "37a2468f055c698158e50cde9b1758a8417e487e"

// Build the immutable prior release and create an actual nonempty schema2 DB
// with its own writer. The new schema must refuse it without migration and
// without changing a byte of the existing database.
func TestUpgradeRejectsActualV070DatabaseWithoutMutation(t *testing.T) {
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
	// Restore the shared candidate path immediately after copying the old
	// library into this isolated host.
	h := func() *harness {
		candidate := pluginBuildRoot
		pluginBuildRoot = build
		defer func() { pluginBuildRoot = candidate }()
		return newHarness(t)
	}()
	created := h.createKey(t, validSpec())
	settings := acceptanceGetSettings(t, h)
	// The baseline only understands the old schema-2 pricing contract.
	settings["pricing"] = map[string]any{
		"auto": false,
		"prices": []any{map[string]any{"model": upstreamModel, "rates": map[string]any{
			"input": 1_000_000, "output": 1_000_000, "cache_read": 1_000_000, "cache_write": 1_000_000,
		}}},
		"rules": []any{},
	}
	h.api(t, http.MethodPut, "/settings", settings, http.StatusOK, nil)
	known := map[string]struct{}{}
	before := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, before, 15, "manual")
	h.awaitGracefulRestartReady(t, 1)
	h.Stop()
	if err := h.waitError(); err != nil {
		t.Fatal("old host did not stop gracefully", err)
	}
	path := h.dbPath
	oldDB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldDB) == 0 {
		t.Fatal("baseline did not write a nonempty database")
	}

	if err = installPlugin(filepath.Join(h.dir, "plugins")); err != nil {
		t.Fatal(err)
	}
	_, startErr := h.restartPreservingConfig(t)
	if startErr == nil {
		t.Fatal("candidate accepted a schema2 database")
	}
	if !strings.Contains(startErr.Error(), "unsupported_schema") &&
		!strings.Contains(startErr.Error(), "unsupported observer") &&
		!strings.Contains(startErr.Error(), "storage unavailable") {
		t.Fatalf("candidate failed for an unexpected reason: %v", startErr)
	}
	afterDB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldDB, afterDB) {
		t.Fatal("candidate mutated the rejected schema2 database")
	}
}
