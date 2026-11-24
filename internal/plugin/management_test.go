package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-ustc/internal/ustc"
)

func callWeb(t *testing.T, runtime *Runtime, method, path, body string) managementResponse {
	t.Helper()
	service, ctx, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	return runtime.handleManagement(ctx, service, rpcRequest{Method: method, Path: path, Body: []byte(body), Headers: http.Header{"Content-Type": {"application/json"}}, HostCallbackID: "test-callback"})
}

func TestManagementMenuAndStaticResources(t *testing.T) {
	runtime := configuredRuntime(t)
	registration := invoke(t, runtime, "management.register", nil)
	raw, _ := json.Marshal(registration)
	if !strings.Contains(string(raw), `"Menu":"中科大词元计划"`) || !strings.Contains(string(raw), `"Path":"/settings"`) {
		t.Fatal("missing CPA browser menu")
	}
	for _, name := range []string{"settings", "settings.js", "settings.css"} {
		response := callWeb(t, runtime, "GET", resourcePrefix+name, "")
		if response.StatusCode != 200 || len(response.Body) == 0 || response.Headers.Get("Cache-Control") != "no-store" {
			t.Fatalf("resource %s: %v", name, response)
		}
		if !strings.Contains(response.Headers.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("missing strict browser policy")
		}
	}
	for _, path := range []string{resourcePrefix + "state", resourcePrefix + "accounts", resourcePrefix + "../management.go", resourcePrefix + "settings/../state"} {
		if response := callWeb(t, runtime, "GET", path, ""); response.StatusCode != 404 {
			t.Fatal("public resources exposed an operation", path)
		}
	}
	response := invoke(t, runtime, "management.handle", map[string]any{"Method": "GET", "Path": resourcePrefix + "settings"})
	if response["ok"] != true {
		t.Fatal("management RPC was not dispatched", response)
	}
}

func TestWebAPIKeyImportsNativeCPAAccountAndRedactsSecrets(t *testing.T) {
	runtime := configuredRuntime(t)
	var saved ustc.Account
	runtime.Host = func(method string, request any, result any) error {
		payload := request.(map[string]any)
		if payload["host_callback_id"] != "test-callback" {
			t.Fatal("lost host callback scope")
		}
		switch method {
		case "host.auth.save":
			if err := json.Unmarshal(payload["json"].(json.RawMessage), &saved); err != nil {
				t.Fatal(err)
			}
			if payload["name"] != saved.ID+".json" || saved.Type != "ustc" || saved.APIKey != "private-test-key" {
				t.Fatal("not saved as CPA provider credential")
			}
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"a","name":"a.json","provider":"ustc","label":"<script>test</script>","status":"error","status_message":"HTTP 429","unavailable":true,"next_retry_after":"2026-11-02T12:00:00Z","api_key":"hidden","path":"/private/host"},{"id":"b","name":"b.json","type":"ustc","disabled":true},{"name":"c.json","provider":"codex"}]}`), result)
		default:
			t.Fatalf("unexpected callback %s", method)
		}
		return nil
	}
	response := callWeb(t, runtime, "POST", managementPrefix+"accounts", `{"api_key":"private-test-key","label":"A"}`)
	if response.StatusCode != 200 || strings.Contains(string(response.Body), "private-test-key") || saved.Label != "A" {
		t.Fatalf("bad import response: %s", response.Body)
	}
	response = callWeb(t, runtime, "GET", managementPrefix+"accounts", "")
	var list struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if json.Unmarshal(response.Body, &list) != nil || len(list.Accounts) != 2 {
		t.Fatalf("CPA accounts not filtered by provider: %s", response.Body)
	}
	if strings.Contains(string(response.Body), "hidden") || strings.Contains(string(response.Body), "/private/host") {
		t.Fatal("account list leaked credentials or host paths")
	}
	if list.Accounts[0]["status_message"] != "HTTP 429" || list.Accounts[0]["unavailable"] != true || list.Accounts[0]["next_retry_after"] != "2026-11-02T12:00:00Z" {
		t.Fatalf("CPA cooldown diagnostics lost: %s", response.Body)
	}
}

func TestWebQuotaSeparatesCredentialFailuresFromManagementAuthentication(t *testing.T) {
	runtime := configuredRuntime(t)
	for _, test := range []struct {
		name, credential, message string
		status                    int
	}{
		{"API key", `{"type":"ustc","id":"ustc-key-test","api_base":"https://api.llm.ustc.edu.cn/v1","api_key":"private-credential"}`, "仅 API Key 无法查询额度", http.StatusNotImplemented},
		{"expired account", `{"type":"ustc","id":"ustc-sso-test","issuer":"https://id.ustc.edu.cn/oidc","client_id":"tokenworks","subject":"test","access_token":"private-credential","expires_at":"2000-01-01T00:00:00Z"}`, "重新添加科大账号", http.StatusUnauthorized},
		{"other provider", `{"type":"codex","id":"codex-test","api_key":"private-credential"}`, "未找到可查询的科大账号", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime.Host = func(method string, request any, result any) error {
				payload := request.(map[string]any)
				if method != "host.auth.get" || payload["auth_index"] != "test-index" || payload["host_callback_id"] != "test-callback" {
					t.Fatalf("unexpected quota credential lookup: %s", method)
				}
				return json.Unmarshal([]byte(`{"json":`+test.credential+`}`), result)
			}
			response := callWeb(t, runtime, "POST", managementPrefix+"accounts/quota", `{"auth_index":"test-index"}`)
			var result map[string]string
			if response.StatusCode != test.status || json.Unmarshal(response.Body, &result) != nil || !strings.Contains(result["error"], test.message) {
				t.Fatalf("HTTP %d: %s", response.StatusCode, response.Body)
			}
			if strings.Contains(string(response.Body), "private-credential") {
				t.Fatal("quota error leaked credential")
			}
			if test.status != http.StatusNotFound && result["code"] != "ustc_quota_error" {
				t.Fatal("provider failure is missing its error code")
			}
		})
	}
}

func TestWebMutationValidation(t *testing.T) {
	runtime := configuredRuntime(t)
	for _, body := range []string{`{}`, `{"api_key":"a","api_key_env":"B"}`, `{"api_key":"a","path":"../overwrite"}`, `{"api_key":"a"} {}`, `{"api_key":"a\nb"}`, strings.Repeat("x", 65537)} {
		if response := callWeb(t, runtime, "POST", managementPrefix+"accounts", body); response.StatusCode != 400 {
			t.Fatalf("accepted invalid input: %s", body[:min(len(body), 100)])
		}
	}
	service, ctx, _ := runtime.current()
	response := runtime.handleManagement(ctx, service, rpcRequest{Method: "POST", Path: managementPrefix + "accounts", Body: []byte(`{"api_key":"a"}`)})
	if response.StatusCode != 400 {
		t.Fatal("accepted non-JSON form submission")
	}
	for _, body := range []string{`{"network":"invalid"}`, `{"api-base":"http://unsafe.test"}`, `{"unknown":"value"}`, `{"budget-cooldown":"0s"}`, `{"budget-cooldown":"invalid"}`} {
		if response := callWeb(t, runtime, "POST", managementPrefix+"validate", body); response.StatusCode != 400 {
			t.Fatal("accepted invalid settings", body)
		}
	}
	if response := callWeb(t, runtime, "POST", managementPrefix+"validate", `{"network":"direct"}`); response.StatusCode != 200 {
		t.Fatal("valid partial configuration rejected")
	}
	if response := callWeb(t, runtime, "POST", managementPrefix+"validate", `{"budget-cooldown":"3h"}`); response.StatusCode != 200 {
		t.Fatal("web cooldown configuration rejected")
	}
}

func TestWebIwanCallbackIsStateBoundAndSingleUse(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	job := &webJob{input: writer, status: "pending"}
	_, _ = job.Write([]byte("do not expose secret\nhttps://auth.ivpn.ustc.edu.cn/login/oauth/authorize?state=expected&code_challenge="))
	if job.snapshot()["url"] != "" {
		t.Fatal("partial authorization URL exposed")
	}
	_, _ = job.Write([]byte("challenge\nPaste redirect URL: "))
	if !strings.HasSuffix(job.snapshot()["url"], "code_challenge=challenge") {
		t.Fatal("authorization URL not captured")
	}
	for _, raw := range []string{"https://evil.test/?state=expected&code=a", "com.panabit.mobile://oauth2redirect?state=wrong&code=a", "com.panabit.mobile://oauth2redirect?state=expected&state=expected&code=a", "com.panabit.mobile://oauth2redirect?state=expected&code=a\nnext"} {
		if job.submit(raw) == nil {
			t.Fatal("accepted invalid callback", raw)
		}
	}
	done := make(chan string, 1)
	go func() {
		buffer := make([]byte, 4096)
		count, _ := reader.Read(buffer)
		done <- string(buffer[:count])
	}()
	callback := "com.panabit.mobile://oauth2redirect?state=expected&code=private-code"
	if err := job.submit(callback); err != nil {
		t.Fatal(err)
	}
	if <-done != callback+"\n" || job.submit(callback) == nil {
		t.Fatal("callback missing or replay accepted")
	}
	raw, _ := json.Marshal(job.snapshot())
	if strings.Contains(string(raw), "private-code") || strings.Contains(string(raw), "secret") {
		t.Fatal("iWAN snapshot leaked helper output")
	}
}

func TestRuntimeCloseCancelsWebJob(t *testing.T) {
	runtime := configuredRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	runtime.webJob = &webJob{cancel: cancel, status: "pending"}
	runtime.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("web job survived plugin shutdown")
	}
}
