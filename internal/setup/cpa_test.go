package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-ustc/internal/ustc"
	"gopkg.in/yaml.v3"
)

func TestConfigurePreservesExistingProvidersAndSecrets(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	original := []byte("# keep this comment\nport: 9999\nauth-dir: ./auth\napi-keys: [existing-client-key]\nplugins:\n  enabled: false\n  dir: existing-plugins\n  configs:\n    another-plugin:\n      enabled: true\nopenai-compatibility:\n  - name: existing-provider\n    base-url: https://existing.example/v1\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	authDir, err := AuthDir(path, "")
	if err != nil || authDir != filepath.Join(directory, "auth") {
		t.Fatalf("auth dir: %s, %v", authDir, err)
	}
	plugins, err := PluginDir(path, "")
	if err != nil || plugins != filepath.Join(directory, "existing-plugins") {
		t.Fatalf("plugin dir: %s, %v", plugins, err)
	}
	config := ustc.DefaultConfig()
	config.StateDir = filepath.Join(directory, "state")
	if err = Configure(path, plugins, authDir, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"keep this comment", "existing-client-key", "another-plugin", "existing-provider", "9999", "network: auto"} {
		if !strings.Contains(string(data), required) {
			t.Fatalf("lost existing setting %q", required)
		}
	}
	backup, _ := filepath.Glob(path + ".cpa-ustc-*.bak")
	if len(backup) != 1 {
		t.Fatal("missing backup")
	}
	contents, _ := os.ReadFile(backup[0])
	if string(contents) != string(original) {
		t.Fatal("backup not byte-for-byte identical")
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm() != 0600 {
		t.Fatal("config permissions widened")
	}
}

func TestNewConfigurationHasPrivateClientKeyAndLoopbackBind(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := Configure(path, "", filepath.Join(directory, "auth"), ustc.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["host"] != "127.0.0.1" {
		t.Fatal("new config listens on all interfaces")
	}
	keys := config["api-keys"].([]any)
	if len(keys) != 1 || len(keys[0].(string)) < 40 {
		t.Fatal("missing strong frontend key")
	}
}

func TestInvalidYAMLAndUnexpectedPluginTypeAreNotOverwritten(t *testing.T) {
	for _, original := range []string{"broken: [", "plugins: unexpected-scalar\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Configure(path, "", "auth", ustc.DefaultConfig()); err == nil {
			t.Fatal("unsafe configuration overwritten")
		}
		data, _ := os.ReadFile(path)
		if string(data) != original {
			t.Fatal("original changed on failure")
		}
	}
}

func TestAuthDirectoryOverride(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	override := filepath.Join(directory, "new-auth")
	if err := os.WriteFile(path, []byte("auth-dir: ./old-auth\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := AuthDir(path, override)
	if err != nil {
		t.Fatal(err)
	}
	if err = Configure(path, "", resolved, ustc.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	resolved, err = AuthDir(path, "")
	if err != nil || resolved != override {
		t.Fatalf("override not applied: %s %v", resolved, err)
	}
	home, _ := os.UserHomeDir()
	if expanded, err := ExpandPath("~"); err != nil || expanded != home {
		t.Fatalf("home expansion: %s %v", expanded, err)
	}
}
