package ustc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}
}

type identityFixture struct {
	service    *Service
	key        *rsa.PrivateKey
	mu         sync.Mutex
	nonce      string
	verifier   string
	refreshes  atomic.Int32
	bootstraps atomic.Int32
}

func newIdentity(t *testing.T) *identityFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Network = "direct"
	config.StateDir = t.TempDir()
	fixture := &identityFixture{service: NewService(config), key: key}
	fixture.service.identity = &http.Client{Transport: roundTripFunc(fixture.handle)}
	t.Cleanup(fixture.service.Close)
	return fixture
}

func (fixture *identityFixture) claims() map[string]any {
	return map[string]any{"iss": Issuer, "sub": "subject-123", "aud": "tokenworks", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": fixture.nonce}
}

func (fixture *identityFixture) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
	body, _ := json.Marshal(claims)
	text := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(text))
	signature, _ := rsa.SignPKCS1v15(rand.Reader, fixture.key, crypto.SHA256, digest[:])
	return text + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (fixture *identityFixture) handle(request *http.Request) (*http.Response, error) {
	if request.URL.String() == contentBase+"/stable/latest.json" || strings.HasPrefix(request.URL.String(), contentBase+"/bundles/") {
		name := "testdata/published-manifest.json"
		if strings.Contains(request.URL.Path, "/bundles/") {
			name = "testdata/published-bundle.json"
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		// The signed bundle's digest covers the exact original bytes, without a trailing LF.
		if strings.Contains(name, "bundle") {
			data = []byte(strings.TrimSuffix(string(data), "\n"))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	}
	switch request.URL.Path {
	case "/oidc/.well-known/openid-configuration":
		return jsonResponse(map[string]any{"issuer": Issuer, "authorization_endpoint": Issuer + "/authorize", "token_endpoint": Issuer + "/token", "userinfo_endpoint": Issuer + "/userinfo", "jwks_uri": Issuer + "/jwks", "authorization_response_iss_parameter_supported": true}), nil
	case "/oidc/jwks":
		return jsonResponse(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(fixture.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(fixture.key.E)).Bytes()), "use": "sig", "alg": "RS256"}}}), nil
	case "/oidc/userinfo":
		return jsonResponse(map[string]string{"sub": "subject-123", "name": "测试用户"}), nil
	case "/oidc/token":
		if err := request.ParseForm(); err != nil {
			return nil, err
		}
		if request.Form.Get("grant_type") == "refresh_token" {
			fixture.refreshes.Add(1)
			return jsonResponse(map[string]any{"access_token": "new-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600}), nil
		}
		fixture.mu.Lock()
		fixture.verifier = request.Form.Get("code_verifier")
		raw := fixture.sign(fixture.claims())
		fixture.mu.Unlock()
		return jsonResponse(map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "id_token": raw, "token_type": "Bearer", "expires_in": 3600}), nil
	case "/ai/api/v1/bootstrap":
		fixture.bootstraps.Add(1)
		return jsonResponse(map[string]any{"inferenceProvider": "gateway", "inferenceCredentialKind": "static", "inferenceGatewayBaseUrl": "https://gateway.ustc.edu.cn", "inferenceGatewayApiKey": "gateway-secret", "inferenceModels": []any{map[string]any{"name": "model-one", "displayName": "测试模型", "contextWindow": 65536, "maxOutputTokens": 4096}, "model-two"}, "expiresAt": time.Now().Add(time.Hour).UnixMilli()}), nil
	default:
		return nil, fmt.Errorf("unexpected request: %s", request.URL)
	}
}

func TestPublishedProfileSignature(t *testing.T) {
	fixture := newIdentity(t)
	if err := fixture.service.CheckPublishedProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.service.identity = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(map[string]any{"schemaVersion": 1, "payload": "e30", "signature": "Zm9yZ2Vk"}), nil
	})}
	if err := fixture.service.CheckPublishedProfile(context.Background()); err == nil {
		t.Fatal("forged publisher signature accepted")
	}
}

func TestOIDCLoginPKCEStateAndIssuer(t *testing.T) {
	fixture := newIdentity(t)
	login, err := fixture.service.StartLogin(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	authorization, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorization.Query()
	fixture.mu.Lock()
	fixture.nonce = query.Get("nonce")
	fixture.mu.Unlock()
	if query.Get("code_challenge_method") != "S256" || query.Get("client_id") != "tokenworks" {
		t.Fatal("missing PKCE or wrong client")
	}
	callback := query.Get("redirect_uri")
	client := &http.Client{Timeout: 5 * time.Second}
	for _, invalid := range []url.Values{{"state": {"wrong"}, "iss": {Issuer}, "code": {"code"}}, {"state": {login.State}, "iss": {"https://attacker.invalid"}, "code": {"code"}}, {"state": {login.State, login.State}, "iss": {Issuer}, "code": {"code"}}} {
		response, requestErr := client.Get(callback + "?" + invalid.Encode())
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("accepted invalid callback: %d", response.StatusCode)
		}
		if _, done, _ := login.Poll(); done {
			t.Fatal("invalid callback consumed login")
		}
	}
	response, err := client.Get(callback + "?" + url.Values{"state": {login.State}, "iss": {Issuer}, "code": {"code"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("valid callback: %d", response.StatusCode)
	}
	account, err := login.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.Subject != "subject-123" || account.RefreshToken != "refresh-secret" {
		t.Fatalf("incorrect account: %+v", account)
	}
	fixture.mu.Lock()
	digest := sha256.Sum256([]byte(fixture.verifier))
	fixture.mu.Unlock()
	if base64.RawURLEncoding.EncodeToString(digest[:]) != query.Get("code_challenge") {
		t.Fatal("PKCE verifier does not match challenge")
	}
}

func TestRemoteWebCallbackUsesOnlySessionLoopback(t *testing.T) {
	fixture := newIdentity(t)
	login, err := fixture.service.StartLogin(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	authorization, _ := url.Parse(login.URL)
	fixture.mu.Lock()
	fixture.nonce = authorization.Query().Get("nonce")
	fixture.mu.Unlock()
	callback, _ := url.Parse(authorization.Query().Get("redirect_uri"))
	query := url.Values{"state": {login.State}, "code": {"web-code"}, "iss": {Issuer}}
	callback.RawQuery = query.Encode()
	for _, raw := range []string{"https://evil.test/oauth/callback?" + query.Encode(), "http://127.0.0.1:1/oauth/callback?" + query.Encode(), callback.String() + "&state=duplicate", callback.String() + "#fragment"} {
		if login.SubmitCallback(context.Background(), raw) == nil {
			t.Fatal("accepted callback outside session boundary", raw)
		}
	}
	if err = login.SubmitCallback(context.Background(), callback.String()); err != nil {
		t.Fatal(err)
	}
	account, done, err := login.Poll()
	if err != nil || !done || account.AccessToken != "access-secret" {
		t.Fatalf("remote login incomplete: %v %v", done, err)
	}
}

func TestAccountRetainsCPARoutingMetadataOnRotation(t *testing.T) {
	var account Account
	if err := json.Unmarshal([]byte(`{"type":"ustc","id":"ustc-sso-test","access_token":"old","refresh_token":"old-refresh","disabled":true,"priority":9,"prefix":"school","weight":3,"custom":{"a":1}}`), &account); err != nil {
		t.Fatal(err)
	}
	account.AccessToken = "new"
	account.RefreshToken = "rotated"
	data, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["disabled"] != true || fields["priority"] != float64(9) || fields["weight"] != float64(3) || fields["prefix"] != "school" || fields["access_token"] != "new" || fields["refresh_token"] != "rotated" {
		t.Fatalf("CPA routing metadata or rotated tokens lost: %s", data)
	}
}

func TestIDTokenRejectsInvalidClaims(t *testing.T) {
	fixture := newIdentity(t)
	fixture.nonce = "expected-nonce"
	document, err := fixture.service.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"issuer", func(claims map[string]any) { claims["iss"] = "https://evil.invalid" }},
		{"audience", func(claims map[string]any) { claims["aud"] = "another-client" }},
		{"multiple-audiences-without-azp", func(claims map[string]any) { claims["aud"] = []string{"tokenworks", "other"} }},
		{"expired", func(claims map[string]any) { claims["exp"] = time.Now().Add(-time.Hour).Unix() }},
		{"nonce", func(claims map[string]any) { claims["nonce"] = "wrong" }},
		{"token-binding", func(claims map[string]any) { claims["at_hash"] = "wrong" }},
		{"future-issued", func(claims map[string]any) { claims["iat"] = time.Now().Add(time.Hour).Unix() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := fixture.claims()
			test.change(claims)
			if _, err := fixture.service.verifyIDToken(context.Background(), document, fixture.sign(claims), fixture.nonce, "access"); err == nil {
				t.Fatal("invalid ID token accepted")
			}
		})
	}
	valid := fixture.sign(fixture.claims())
	if _, err = fixture.service.verifyIDToken(context.Background(), document, valid, fixture.nonce, "access"); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, ".")
	parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
	if _, err = fixture.service.verifyIDToken(context.Background(), document, strings.Join(parts, "."), fixture.nonce, "access"); err == nil {
		t.Fatal("invalid signature accepted")
	}
}

func oauthAccount() Account {
	return Account{Type: Provider, ID: "ustc-sso-test", Label: "Test", AccessToken: "old-access", RefreshToken: "old-refresh", Issuer: Issuer, ClientID: "tokenworks", Subject: "subject-123", ExpiresAt: time.Now().Add(-time.Minute)}
}

func TestConcurrentRefreshRotatesOnlyOnce(t *testing.T) {
	fixture := newIdentity(t)
	account := oauthAccount()
	var workers sync.WaitGroup
	for index := 0; index < 16; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			gateway, updated, err := fixture.service.Resolve(context.Background(), account)
			if err != nil {
				t.Error(err)
				return
			}
			if gateway.Key != "gateway-secret" || updated.RefreshToken != "rotated-refresh" || len(gateway.Models) != 2 {
				t.Error("incorrect refreshed account or bootstrap")
			}
		}()
	}
	workers.Wait()
	if fixture.refreshes.Load() != 1 || fixture.bootstraps.Load() != 1 {
		t.Fatalf("duplicate requests: refresh=%d bootstrap=%d", fixture.refreshes.Load(), fixture.bootstraps.Load())
	}
	data, _ := json.Marshal(fixture.service.cached(account).account)
	if strings.Contains(string(data), "gateway-secret") {
		t.Fatal("gateway key persisted in account")
	}
}

func TestBootstrapRejectsExternalGatewayAndExpiredConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		base    string
		expires int64
	}{{"external", "https://evil.invalid", time.Now().Add(time.Hour).Unix()}, {"expired", "https://api.llm.ustc.edu.cn", time.Now().Add(-time.Hour).Unix()}} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIdentity(t)
			fixture.service.identity = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(map[string]any{"inferenceProvider": "gateway", "inferenceGatewayBaseUrl": test.base, "inferenceGatewayApiKey": "never-print-this-secret", "inferenceModels": []string{"model"}, "expiresAt": test.expires}), nil
			})}
			account := oauthAccount()
			account.ExpiresAt = time.Now().Add(time.Hour)
			_, _, err := fixture.service.Resolve(context.Background(), account)
			if err == nil {
				t.Fatal("unsafe bootstrap accepted")
			}
			if strings.Contains(err.Error(), "never-print-this-secret") {
				t.Fatal("error leaked key")
			}
		})
	}
}

func TestFreshLoginReplacesCachedToken(t *testing.T) {
	fixture := newIdentity(t)
	account := oauthAccount()
	_, old, err := fixture.service.Resolve(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.AccessToken = "fresh-login"
	fresh.RefreshToken = "fresh-refresh"
	fresh.ExpiresAt = time.Now().Add(2 * time.Hour)
	_, updated, err := fixture.service.Resolve(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "fresh-login" || updated.RefreshToken != "fresh-refresh" {
		t.Fatal("new login did not supersede old cache")
	}
	_, updated, err = fixture.service.Resolve(context.Background(), old)
	if err != nil || updated.AccessToken != "fresh-login" {
		t.Fatal("stale host snapshot reverted a fresh login")
	}
}

func TestPublishedProfileLive(t *testing.T) {
	if os.Getenv("CPA_USTC_LIVE") != "1" {
		t.Skip("set CPA_USTC_LIVE=1 to check public USTC metadata; no login or inference")
	}
	service := NewService(DefaultConfig())
	defer service.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.CheckPublishedProfile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.discover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := OnCampus(ctx); err != nil {
		t.Fatal(err)
	}
}
