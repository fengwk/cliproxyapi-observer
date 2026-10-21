package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Keep clean-slate defaults executable: obsolete YAML must not sneak back in.
func TestDeliveryCleanSlateConfiguration(t *testing.T) {
	raw, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Plugins struct {
			Configs map[string]map[string]any `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	opts := config.Plugins.Configs["cliproxyapi-observer"]
	if len(opts) != 2 || opts["enabled"] != true || opts["db"] != "data/cliproxyapi-observer-v2.db" {
		t.Fatalf("expected only enabled/new database, got %#v", opts)
	}
	if _, err := os.Stat("internal/observer"); !os.IsNotExist(err) {
		t.Fatalf("legacy backend must be absent: %v", err)
	}
}

// A release override must be independent of the host SDK and reject bad input.
func TestDeliveryBuildVersion(t *testing.T) {
	script, err := filepath.Abs("scripts/plugin-version.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Outside a Git checkout the fallback must still identify a development build.
	fallback := exec.Command("bash", script)
	fallback.Dir = t.TempDir()
	fallback.Env = append(os.Environ(), "PLUGIN_VERSION=")
	out, err := fallback.Output()
	if err != nil || strings.TrimSpace(string(out)) != "0.9.2-dev" {
		t.Fatalf("fallback version %q, error %v", out, err)
	}
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"v0.7.0", "0.7.0"},
		{"v0.8.0", "0.8.0"},
		{"v0.9.0", "0.9.0"},
		{"v0.9.1", "0.9.1"},
		{"v0.9.2", "0.9.2"},
		{"0.9.2-dev+test", "0.9.2-dev+test"},
		{"not-a-version", ""},
	} {
		cmd := exec.Command("bash", "scripts/plugin-version.sh")
		for _, env := range os.Environ() {
			if !strings.HasPrefix(env, "PLUGIN_VERSION=") {
				cmd.Env = append(cmd.Env, env)
			}
		}
		cmd.Env = append(cmd.Env, "PLUGIN_VERSION="+tc.input)
		out, err := cmd.Output()
		if tc.want == "" {
			if err == nil {
				t.Fatal("invalid version accepted")
			}
		} else if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Fatalf("%q: output %q, error %v", tc.input, out, err)
		}
	}
}
