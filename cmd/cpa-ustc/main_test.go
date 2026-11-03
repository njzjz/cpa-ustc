package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-ustc/internal/setup"
)

// os.UserConfigDir uses different environment variables on each native runner.
// Keep every setup command isolated from developer and runner configuration.
func setTestConfigHome(t *testing.T, directory string) {
	t.Helper()
	for _, name := range []string{"XDG_CONFIG_HOME", "HOME", "APPDATA"} {
		t.Setenv(name, directory)
	}
}

func TestAPIKeySetupEndToEnd(t *testing.T) {
	directory := t.TempDir()
	setTestConfigHome(t, filepath.Join(directory, "xdg"))
	t.Setenv("USTC_SETUP_TEST_KEY", "private-key-do-not-store")
	source := filepath.Join(directory, "source.so")
	if err := os.WriteFile(source, []byte("test library content"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	state := filepath.Join(directory, "state")
	auth := filepath.Join(directory, "auth")
	if err := run([]string{"setup", "--api-key-env", "USTC_SETUP_TEST_KEY", "--network", "direct", "--state-dir", state, "--auth-dir", auth, "--plugin-source", source, "--cpa-config", configPath}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(auth, "ustc-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("missing auth: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-key-do-not-store") || !strings.Contains(string(data), "USTC_SETUP_TEST_KEY") {
		t.Fatal("environment key not stored as a reference")
	}
	var record map[string]any
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record["type"] != "ustc" {
		t.Fatal("wrong provider type")
	}
	if _, err = os.Stat(filepath.Join(directory, "plugins", setup.LibraryName())); err != nil {
		t.Fatal("library not installed:", err)
	}
	if _, err = os.Stat(filepath.Join(state, "config.json")); err != nil {
		t.Fatal("settings not saved:", err)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "private-key-do-not-store") || !strings.Contains(string(config), "network: direct") {
		t.Fatal("configuration contains secret or incorrect network")
	}
}

func TestSetupFailsBeforeAuthenticationWithoutLibrary(t *testing.T) {
	directory := t.TempDir()
	setTestConfigHome(t, directory)
	err := run([]string{"setup", "--plugin-source", filepath.Join(directory, "missing.so"), "--state-dir", filepath.Join(directory, "state")})
	if err == nil || !strings.Contains(err.Error(), "动态库") {
		t.Fatalf("missing source not detected early: %v", err)
	}
	if _, err = os.Stat(filepath.Join(directory, "state")); !os.IsNotExist(err) {
		t.Fatal("failed preflight modified user settings")
	}
}

func TestInstallAllowsWebOnlyOnboarding(t *testing.T) {
	directory := t.TempDir()
	setTestConfigHome(t, directory)
	source := filepath.Join(directory, "source.so")
	if err := os.WriteFile(source, []byte("test library"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	authDir := filepath.Join(directory, "auth")
	if err := run([]string{"install", "--cpa-config", configPath, "--auth-dir", authDir, "--plugin-source", source, "--api-base", "https://example.test/v1"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(authDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("install unexpectedly created credentials", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(data), "enabled: true") || !strings.Contains(string(data), "https://example.test/v1") {
		t.Fatalf("web-only install did not configure CPA: %v\n%s", err, data)
	}
}

func TestConflictingCredentialFlags(t *testing.T) {
	setTestConfigHome(t, t.TempDir())
	if err := run([]string{"login", "--api-key-env", "USTC_KEY", "--api-key-stdin"}); err == nil {
		t.Fatal("conflicting modes accepted")
	}
}

func TestDonationGatewayCLIRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "")
	if err := run([]string{"serve-donations", "--origin", "https://donate.example"}); err == nil {
		t.Fatal("missing management key accepted")
	}
	t.Setenv("CPA_MANAGEMENT_KEY", "server-private-test-key")
	for _, arguments := range [][]string{
		{"--origin", "http://donate.example"},
		{"--origin", "https://donate.example", "--cpa-url", "http://remote.example"},
		{"--origin", "https://donate.example", "unexpected"},
	} {
		err := run(append([]string{"serve-donations"}, arguments...))
		if err == nil || strings.Contains(err.Error(), "server-private-test-key") {
			t.Fatal("invalid gateway configuration accepted or key leaked", err)
		}
	}
}
