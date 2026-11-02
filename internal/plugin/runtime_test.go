package plugin

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cpa-ustc/internal/ustc"
)

func invoke(t *testing.T, runtime *Runtime, method string, request any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(request)
	var envelope map[string]any
	if err := json.Unmarshal(runtime.Dispatch(method, data), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func configuredRuntime(t *testing.T) *Runtime {
	t.Helper()
	runtime := New(nil)
	t.Cleanup(runtime.Close)
	config := []byte("network: direct\nstate-dir: " + filepath.ToSlash(t.TempDir()) + "\n")
	response := invoke(t, runtime, "plugin.register", map[string]any{"schema_version": 1, "config_yaml": config})
	if response["ok"] != true {
		t.Fatalf("register failed: %v", response)
	}
	capabilities := response["result"].(map[string]any)["capabilities"].(map[string]any)
	if capabilities["executor_model_scope"] != "oauth" {
		t.Fatal("auth-bound model scope does not match CPA ABI")
	}
	if capabilities["quota_provider"] != true {
		t.Fatal("native quota provider capability missing")
	}
	return runtime
}

func TestAuthParseAndUnknownCredentials(t *testing.T) {
	runtime := configuredRuntime(t)
	if response := invoke(t, runtime, "auth.parse", map[string]any{"RawJSON": []byte(`{"type":"codex"}`)}); response["result"].(map[string]any)["Handled"] != false {
		t.Fatal("claimed another provider's auth")
	}
	config := ustc.DefaultConfig()
	account, _ := ustc.APIAccount("secret", "", config)
	data, _ := json.Marshal(account)
	response := invoke(t, runtime, "auth.parse", map[string]any{"RawJSON": data})
	if response["ok"] != true {
		t.Fatal(response)
	}
	auth := response["result"].(map[string]any)["Auth"].(map[string]any)
	stored, err := base64.StdEncoding.DecodeString(auth["StorageJSON"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(data) || auth["Provider"] != "ustc" {
		t.Fatal("auth was not preserved")
	}
	if response := invoke(t, runtime, "executor.count_tokens", nil); response["error"].(map[string]any)["http_status"] != float64(501) {
		t.Fatal("token counting should not fabricate a number")
	}
}

func TestProtocolAndRPCValidation(t *testing.T) {
	runtime := configuredRuntime(t)
	response := invoke(t, runtime, "executor.execute", map[string]any{"Format": "claude"})
	if response["ok"] != false || response["error"].(map[string]any)["http_status"] != float64(400) {
		t.Fatal(response)
	}
	if data := runtime.Dispatch("auth.parse", []byte("{broken")); !strings.Contains(string(data), `"ok":false`) {
		t.Fatal("malformed RPC accepted")
	}
	response = invoke(t, runtime, "plugin.reconfigure", map[string]any{"config_yaml": []byte("network: invalid")})
	if response["ok"] != false {
		t.Fatal("invalid configuration accepted")
	}
	if response = invoke(t, runtime, "model.static", nil); response["ok"] != true {
		t.Fatal("failed reconfigure discarded working runtime")
	}
}

func TestAuthParseExposesCPARoutingMetadata(t *testing.T) {
	runtime := configuredRuntime(t)
	raw := []byte(`{"type":"ustc","id":"ustc-key-metadata","label":"A","api_key":"secret","api_base":"https://api.llm.ustc.edu.cn/v1","disabled":true,"prefix":"school","priority":8,"weight":2}`)
	response := invoke(t, runtime, "auth.parse", map[string]any{"RawJSON": raw})
	auth := response["result"].(map[string]any)["Auth"].(map[string]any)
	metadata := auth["Metadata"].(map[string]any)
	if auth["Disabled"] != true || auth["Prefix"] != "school" || metadata["priority"] != float64(8) || metadata["weight"] != float64(2) {
		t.Fatal("provider did not expose native CPA routing metadata")
	}
	if _, exists := metadata["api_key"]; exists {
		t.Fatal("provider token exposed in mutable routing metadata")
	}
}

func TestIdenticalReconfigureKeepsActiveService(t *testing.T) {
	runtime := configuredRuntime(t)
	service, _, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	config := []byte("network: direct\nstate-dir: " + filepath.ToSlash(service.Config.StateDir) + "\n")
	response := invoke(t, runtime, "plugin.reconfigure", map[string]any{"config_yaml": config})
	if response["ok"] != true {
		t.Fatal(response)
	}
	updated, _, err := runtime.current()
	if err != nil || updated != service {
		t.Fatal("identical reconfigure cancelled live authentication/streams")
	}
}

func TestConcurrentReconfigureAndCalls(t *testing.T) {
	runtime := configuredRuntime(t)
	var workers sync.WaitGroup
	for index := 0; index < 20; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for count := 0; count < 10; count++ {
				data := runtime.Dispatch("model.static", nil)
				if !strings.Contains(string(data), `"ok":true`) {
					t.Error(string(data))
				}
			}
		}()
	}
	for count := 0; count < 5; count++ {
		response := invoke(t, runtime, "plugin.reconfigure", map[string]any{"config_yaml": []byte("network: direct")})
		if response["ok"] != true {
			t.Fatal(response)
		}
	}
	workers.Wait()
}

func TestRotatedTokenIsSavedUsingHostAuthAPI(t *testing.T) {
	previous := ustc.Account{Type: "ustc", ID: "ustc-sso-test", AccessToken: "old", RefreshToken: "old-refresh"}
	updated := previous
	updated.AccessToken = "new"
	updated.RefreshToken = "rotated"
	calls := 0
	runtime := New(func(method string, request any, _ any) error {
		calls++
		if method != "host.auth.save" {
			t.Fatalf("unexpected callback %s", method)
		}
		record := request.(map[string]any)
		if record["name"] != "ustc-sso-test.json" || !strings.Contains(string(record["json"].(json.RawMessage)), `"refresh_token":"rotated"`) {
			t.Fatal("rotated refresh token not persisted")
		}
		return nil
	})
	defer runtime.Close()
	if err := runtime.persistRotated(previous, updated); err != nil {
		t.Fatal(err)
	}
	if err := runtime.persistRotated(updated, updated); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unchanged tokens caused redundant persistence")
	}
}
