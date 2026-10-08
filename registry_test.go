package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

// The plugin's canonical identity is owned by the native/RPC slice
// (internal/plugin). This file deliberately does not import it so the
// automation slice builds independently; instead the registry is cross-checked
// against go.mod and config.example.yaml, the contracts this slice does own.
const (
	expectedPluginName = "Observer"
	expectedRepository = "https://github.com/fengwk/cliproxyapi-observer"
)

// modulePath returns the module directive from the repository go.mod.
func modulePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}

// Validate the public source with CPA's parser so SDK updates check the
// contract, and tie the declared identity to the module path and the example
// configuration so a registry or config drift cannot slip through silently.
func TestCustomRegistryUsesCPAStoreContract(t *testing.T) {
	module := modulePath(t)
	if module != "github.com/fengwk/cliproxyapi-observer" {
		t.Fatalf("unexpected module path %q", module)
	}
	// The plugin ID is the last path element of the module.
	pluginID := module[strings.LastIndex(module, "/")+1:]

	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/registry.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	sourceURL := server.URL + "/registry.json"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry, err := pluginstore.NewClient(server.Client(), sourceURL).FetchRegistry(ctx)
	if err != nil {
		t.Fatalf("CPA rejected registry: %v", err)
	}
	if registry.SchemaVersion != pluginstore.SchemaVersion || len(registry.Plugins) != 1 {
		t.Fatalf("unexpected registry: %#v", registry)
	}
	plugin := registry.Plugins[0]
	if plugin.ID != pluginID {
		t.Fatalf("registry ID %q differs from the module-derived plugin ID %q", plugin.ID, pluginID)
	}
	if plugin.Name != expectedPluginName {
		t.Fatalf("registry name %q differs from the plugin display name %q", plugin.Name, expectedPluginName)
	}
	if plugin.Name == plugin.ID {
		t.Fatalf("registry name must be a human-readable display name, not the technical ID %q", plugin.ID)
	}
	if plugin.Repository != expectedRepository || plugin.Repository != "https://"+module {
		t.Fatalf("unexpected repository %q", plugin.Repository)
	}
	if pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeGitHubRelease || plugin.Version != "" {
		t.Fatal("source must resolve the latest GitHub Release without a pinned version")
	}
	sources, err := pluginstore.NormalizeSources([]string{sourceURL})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].ID != pluginstore.DefaultSourceID || sources[1].URL != sourceURL {
		t.Fatalf("custom source must preserve the official source: %#v", sources)
	}

	// The example configuration must enable the same plugin ID and point at this
	// registry, so the documented install path cannot diverge from the source.
	var example struct {
		Plugins struct {
			Configs      map[string]map[string]interface{} `yaml:"configs"`
			StoreSources []string                          `yaml:"store-sources"`
		} `yaml:"plugins"`
	}
	raw, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &example); err != nil {
		t.Fatalf("config.example.yaml is not valid YAML: %v", err)
	}
	if _, ok := example.Plugins.Configs[plugin.ID]; !ok {
		t.Fatalf("config.example.yaml does not configure plugin %q", plugin.ID)
	}
	expectedSource := "https://raw.githubusercontent.com/" +
		strings.TrimPrefix(module, "github.com/") + "/main/registry.json"
	if len(example.Plugins.StoreSources) != 1 || example.Plugins.StoreSources[0] != expectedSource {
		t.Fatalf("config.example.yaml must reference %q, got %v", expectedSource, example.Plugins.StoreSources)
	}
}
