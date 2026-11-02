package ustc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func deviceFixture(t *testing.T) (*identityFixture, *time.Time, *atomic.Int32, *string) {
	t.Helper()
	fixture := newIdentity(t)
	now := time.Now()
	fixture.service.now = func() time.Time { return now }
	var polls atomic.Int32
	state := "authorization_pending"
	fixture.service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/oidc/.well-known/openid-configuration":
			return jsonResponse(map[string]any{"issuer": Issuer, "authorization_endpoint": Issuer + "/authorize", "token_endpoint": Issuer + "/token", "userinfo_endpoint": Issuer + "/userinfo", "jwks_uri": Issuer + "/jwks", "device_authorization_endpoint": Issuer + "/device_authorization"}), nil
		case "/oidc/device_authorization":
			_ = request.ParseForm()
			if request.Form.Get("client_id") != "tokenworks" || request.Form.Get("scope") != "openid profile email offline_access" || request.Form.Get("client_secret") != "" {
				t.Error("incorrect public device client")
			}
			return jsonResponse(map[string]any{"device_code": "private-device-secret", "user_code": "PUBLIC-CODE", "verification_uri": Issuer + "/device", "verification_uri_complete": Issuer + "/device?user_code=PUBLIC-CODE", "expires_in": 600, "interval": 5}), nil
		case "/oidc/token":
			_ = request.ParseForm()
			if request.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || request.Form.Get("device_code") != "private-device-secret" || request.Form.Get("client_id") != "tokenworks" {
				t.Error("incorrect device exchange or browser-supplied token")
			}
			polls.Add(1)
			if state != "ready" {
				response := jsonResponse(map[string]string{"error": state, "error_description": "do not leak private-device-secret"})
				response.StatusCode = 400
				return response, nil
			}
		}
		return fixture.handle(request)
	})
	return fixture, &now, &polls, &state
}

func TestDeviceFlowIntervalsIdentityAndSingleUse(t *testing.T) {
	fixture, now, polls, state := deviceFixture(t)
	login, err := fixture.service.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	raw, _ := json.Marshal(login)
	if strings.Contains(string(raw), "private-device-secret") || login.UserCode != "PUBLIC-CODE" || login.Interval != 5 {
		t.Fatal("device credential escaped into public state")
	}
	if _, ready, err := fixture.service.PollDeviceLogin(context.Background(), login); ready || err != nil || polls.Load() != 0 {
		t.Fatal("polled before school's interval")
	}
	*now = now.Add(5 * time.Second)
	if _, ready, err := fixture.service.PollDeviceLogin(context.Background(), login); ready || err != nil || polls.Load() != 1 {
		t.Fatal("pending handling failed")
	}
	*state = "slow_down"
	*now = now.Add(5 * time.Second)
	_, _, _ = fixture.service.PollDeviceLogin(context.Background(), login)
	if login.Interval != 10 {
		t.Fatal("slow_down was ignored")
	}
	*now = now.Add(5 * time.Second)
	_, _, _ = fixture.service.PollDeviceLogin(context.Background(), login)
	if polls.Load() != 2 {
		t.Fatal("slow_down interval was not enforced")
	}
	*now = now.Add(5 * time.Second)
	*state = "ready"
	account, ready, err := fixture.service.PollDeviceLogin(context.Background(), login)
	if err != nil || !ready || account.Validate() != nil || account.Subject != "subject-123" || account.RefreshToken != "refresh-secret" {
		t.Fatalf("invalid device identity: ready=%v error=%v", ready, err)
	}
	if _, _, err = fixture.service.PollDeviceLogin(context.Background(), login); err == nil || polls.Load() != 3 {
		t.Fatal("device exchange replayed")
	}
}

func TestDeviceFlowDenialExpiryAndUntrustedLinks(t *testing.T) {
	for _, failure := range []string{"access_denied", "expired_token", "unknown_secret_error"} {
		t.Run(failure, func(t *testing.T) {
			fixture, now, polls, state := deviceFixture(t)
			login, err := fixture.service.StartDeviceLogin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			*now = now.Add(5 * time.Second)
			*state = failure
			_, _, err = fixture.service.PollDeviceLogin(context.Background(), login)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("denial not safely handled")
			}
			_, _, _ = fixture.service.PollDeviceLogin(context.Background(), login)
			if polls.Load() != 1 {
				t.Fatal("terminal denial was retried")
			}
		})
	}
	fixture, now, polls, _ := deviceFixture(t)
	login, err := fixture.service.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	*now = login.ExpiresAt
	if _, _, err = fixture.service.PollDeviceLogin(context.Background(), login); err == nil || polls.Load() != 0 {
		t.Fatal("expired session contacted school")
	}
	for _, link := range []string{"https://evil.example/device", "http://id.ustc.edu.cn/device", "https://id.ustc.edu.cn@evil.example/", "https://id.ustc.edu.cn/device?device_code=private-device-secret"} {
		t.Run(link, func(t *testing.T) {
			fixture, _, _, _ := deviceFixture(t)
			original := fixture.service.identity.Transport
			fixture.service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/oidc/device_authorization" {
					return jsonResponse(map[string]any{"device_code": "private-device-secret", "user_code": "PUBLIC", "verification_uri": link, "expires_in": 600}), nil
				}
				return original.RoundTrip(request)
			})
			if _, err := fixture.service.StartDeviceLogin(context.Background()); err == nil {
				t.Fatal("unsafe authorization URL accepted")
			}
		})
	}
}

func TestDeviceFlowRejectsUnverifiedOrIncompleteTokens(t *testing.T) {
	for _, variant := range []string{"no-refresh", "forged-id", "userinfo-mismatch", "old-id", "oversized"} {
		t.Run(variant, func(t *testing.T) {
			fixture, now, _, state := deviceFixture(t)
			original := fixture.service.identity.Transport
			fixture.service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if variant == "userinfo-mismatch" && request.URL.Path == "/oidc/userinfo" {
					return jsonResponse(map[string]string{"sub": "somebody-else"}), nil
				}
				if request.URL.Path != "/oidc/token" {
					return original.RoundTrip(request)
				}
				if variant == "oversized" {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 65537)))}, nil
				}
				claims := fixture.claims()
				if variant == "old-id" {
					claims["iat"] = now.Add(-time.Hour).Unix()
				}
				raw := fixture.sign(claims)
				if variant == "forged-id" {
					raw += "forged"
				}
				refresh := "refresh-secret"
				if variant == "no-refresh" {
					refresh = ""
				}
				return jsonResponse(map[string]any{"access_token": "access-secret", "refresh_token": refresh, "id_token": raw, "token_type": "Bearer", "expires_in": 3600}), nil
			})
			login, err := fixture.service.StartDeviceLogin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			*now = now.Add(5 * time.Second)
			*state = "ready"
			if _, ready, err := fixture.service.PollDeviceLogin(context.Background(), login); ready || err == nil {
				t.Fatal("unverified device token accepted")
			}
		})
	}
}

func TestDonationConfigAndSecretFreeTombstone(t *testing.T) {
	config := DefaultConfig()
	if config.DonationMode != "disabled" {
		t.Fatal("public donations enabled by default")
	}
	config.DonationMode = "open"
	for _, origin := range []string{"", "http://public.example", "https://user:secret@public.example", "https://public.example/path", "https://public.example?key=a", "https://public.example#fragment"} {
		config.DonationOrigin = origin
		if config.Validate() == nil {
			t.Fatal("invalid donation origin accepted", origin)
		}
	}
	for _, origin := range []string{"https://public.example", "https://public.example:8443/", "http://127.0.0.1:8317"} {
		config.DonationOrigin = origin
		if config.Validate() != nil {
			t.Fatal("valid donation origin rejected", origin)
		}
	}
	account := Account{Type: Provider, ID: "ustc-donation-test", Donated: true, DonationRevoked: true, Issuer: Issuer, ClientID: "tokenworks", Subject: "test", CPAFields: map[string]json.RawMessage{"disabled": json.RawMessage("true")}}
	if account.Validate() != nil {
		t.Fatal("secret-free withdrawal could not be loaded")
	}
	service := NewService(DefaultConfig())
	defer service.Close()
	if _, _, err := service.Resolve(context.Background(), account); err == nil {
		t.Fatal("withdrawn donation could execute")
	}
	if _, _, err := service.FetchQuota(context.Background(), account); err == nil {
		t.Fatal("withdrawn donation could refresh identity tokens")
	}
	account.CPAFields["disabled"] = json.RawMessage("false")
	if account.Validate() == nil {
		t.Fatal("withdrawn donation could be re-enabled")
	}
}

func TestDonationOwnershipSurvivesCachedTokenRotation(t *testing.T) {
	fixture := newIdentity(t)
	account := Account{Type: Provider, ID: "ustc-donation-rotation", Subject: "subject-123", Issuer: Issuer, ClientID: "tokenworks", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	entry := fixture.service.cached(account)
	account.Donated = true
	account.CPAFields = map[string]json.RawMessage{"donation_consent_version": json.RawMessage("1")}
	entry.mu.Lock()
	updated, err := fixture.service.oauthAccount(context.Background(), entry, account)
	entry.mu.Unlock()
	if err != nil || !updated.Donated || updated.AccessToken != "new-access" || string(updated.CPAFields["donation_consent_version"]) != "1" {
		t.Fatal("token rotation erased donation ownership")
	}
	entry.gateway = Gateway{Key: "cached-private-key"}
	fixture.service.ForgetAccount(account.ID)
	if len(fixture.service.accounts) != 0 || entry.account.AccessToken != "" || entry.account.RefreshToken != "" || entry.gateway.Key != "" {
		t.Fatal("withdrawal retained cached credentials")
	}
}
