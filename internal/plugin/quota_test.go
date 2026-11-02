package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cpa-ustc/internal/ustc"
)

func TestQuotaRegistrationAndRPCStorage(t *testing.T) {
	runtime := configuredRuntime(t)
	response := invoke(t, runtime, "quota.describe", nil)
	description := response["result"].(map[string]any)
	if description["supports_reset"] != false || description["supported_providers"].([]any)[0] != "ustc" {
		t.Fatal(response)
	}
	account, _ := ustc.APIAccount("quota-secret", "", ustc.DefaultConfig())
	storage, _ := json.Marshal(account)
	response = invoke(t, runtime, "quota.fetch", map[string]any{"provider": "ustc", "storage_json": storage})
	if response["ok"] != false || response["error"].(map[string]any)["http_status"] != float64(501) {
		t.Fatalf("snake_case storage was not decoded: %v", response)
	}
	if response = invoke(t, runtime, "quota.fetch", map[string]any{"provider": "codex", "storage_json": storage}); response["ok"] != false {
		t.Fatal("claimed another provider's credential")
	}
	if response = invoke(t, runtime, "quota.reset", nil); response["error"].(map[string]any)["http_status"] != float64(501) {
		t.Fatal("quota reset must not mutate school budgets")
	}
}

func TestQuotaRPCPersistsRotationEvenOnFailure(t *testing.T) {
	body, err := os.ReadFile("../ustc/testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	partialBody, err := os.ReadFile("../ustc/testdata/usage-partial.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		body      []byte
		failUsage bool
		groups    int
		metrics   int
	}{
		{"success", body, false, 2, 3},
		{"fresh partial", partialBody, false, 1, 2},
		{"failure", nil, true, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/oidc/.well-known/openid-configuration":
					_ = json.NewEncoder(writer).Encode(map[string]any{"issuer": ustc.Issuer, "authorization_endpoint": ustc.Issuer + "/authorize", "token_endpoint": ustc.Issuer + "/token", "userinfo_endpoint": ustc.Issuer + "/userinfo", "jwks_uri": ustc.Issuer + "/jwks"})
				case "/oidc/token":
					_, _ = io.WriteString(writer, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`)
				case "/ai/api/v1/usage":
					if request.Header.Get("Authorization") != "Bearer rotated-access" {
						t.Error("usage did not use rotated identity token")
					}
					if test.failUsage {
						writer.WriteHeader(503)
						_, _ = io.WriteString(writer, "rotated-access must not leak")
						return
					}
					_, _ = writer.Write(test.body)
				default:
					t.Errorf("quota queried an unexpected endpoint: %s", request.URL.Path)
					writer.WriteHeader(404)
				}
			}))
			defer server.Close()
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = server.Certificate().DNSNames[0]
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			defer transport.CloseIdleConnections()
			// Clone the test CA transport at service creation; preserve TLS
			// verification while routing only this runtime to the fixture.
			runtime := func() *Runtime {
				previous := http.DefaultTransport
				http.DefaultTransport = transport
				defer func() { http.DefaultTransport = previous }()
				return configuredRuntime(t)
			}()
			var saved map[string]any
			runtime.Host = func(method string, request any, response any) error {
				if method != "host.auth.save" {
					t.Fatalf("unexpected callback %s", method)
				}
				raw := request.(map[string]any)["json"].(json.RawMessage)
				return json.Unmarshal(raw, &saved)
			}
			account := ustc.Account{Type: "ustc", ID: "ustc-sso-quota", AccessToken: "old-access", RefreshToken: "old-refresh", Issuer: ustc.Issuer, Subject: "test-subject", ClientID: "tokenworks", ExpiresAt: time.Now().Add(-time.Minute), CPAFields: map[string]json.RawMessage{"priority": json.RawMessage("9"), "disabled": json.RawMessage("true")}}
			storage, _ := json.Marshal(account)
			response := invoke(t, runtime, "quota.fetch", map[string]any{"auth_index": "test-index", "auth_id": account.ID, "provider": "ustc", "storage_json": storage})
			if response["ok"] != !test.failUsage {
				t.Fatal(response)
			}
			if saved["refresh_token"] != "rotated-refresh" || saved["priority"] != float64(9) || saved["disabled"] != true {
				t.Fatalf("rotation lost tokens or routing metadata: %v", saved)
			}
			encoded, _ := json.Marshal(response)
			if strings.Contains(string(encoded), "rotated-access") || strings.Contains(string(encoded), "rotated-refresh") {
				t.Fatal("quota response leaked credential")
			}
			if !test.failUsage {
				quota := response["result"].(map[string]any)
				if len(quota["groups"].([]any)) != test.groups || len(quota["summary"].([]any)) != test.metrics {
					t.Fatal("normalized quota did not survive RPC encoding")
				}
			}
		})
	}
}
