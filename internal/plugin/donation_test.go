package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-ustc/internal/ustc"
)

type donationStore struct {
	mu       sync.Mutex
	files    map[string]json.RawMessage
	writes   int
	calls    int
	failSave bool
}

func (store *donationStore) host(method string, request any, response any) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.calls++
	input := request.(map[string]any)
	if input["host_callback_id"] != "donation-scope" {
		return errors.New("host callback escaped request scope")
	}
	var result any
	switch method {
	case "host.auth.list":
		files := []map[string]string{}
		for name := range store.files {
			files = append(files, map[string]string{"name": name, "auth_index": name})
		}
		result = map[string]any{"files": files}
	case "host.auth.get":
		name := input["auth_index"].(string)
		if store.files[name] == nil {
			return errors.New("not found")
		}
		result = map[string]any{"name": name, "json": store.files[name]}
	case "host.auth.save":
		if store.failSave {
			return errors.New("secret host error should not escape")
		}
		store.writes++
		store.files[input["name"].(string)] = append(json.RawMessage(nil), input["json"].(json.RawMessage)...)
		return nil
	default:
		return errors.New("unexpected public host capability")
	}
	raw, _ := json.Marshal(result)
	return json.Unmarshal(raw, response)
}

func donationAccount() ustc.Account {
	return ustc.Account{Type: "ustc", ID: "ustc-sso-donor", Label: "捐献者 <img src=x onerror=alert(1)>", Subject: "donor-subject", Issuer: ustc.Issuer, ClientID: "tokenworks", AccessToken: "access-private-secret", RefreshToken: "refresh-private-secret", ExpiresAt: time.Now().Add(time.Hour)}
}

func donatedRuntime(t *testing.T) (*Runtime, *donationStore) {
	t.Helper()
	runtime := configuredRuntime(t)
	response := invoke(t, runtime, "plugin.reconfigure", map[string]any{"config_yaml": []byte("network: direct\nstate-dir: " + t.TempDir() + "\ndonation-mode: open\ndonation-origin: https://cpa.example\n")})
	if response["ok"] != true {
		t.Fatal(response)
	}
	store := &donationStore{files: map[string]json.RawMessage{}}
	runtime.Host = store.host
	runtime.donations.start = func(context.Context) (*ustc.DeviceLogin, error) {
		return &ustc.DeviceLogin{VerificationURL: ustc.Issuer + "/device?user_code=PUBLIC", UserCode: "PUBLIC", ExpiresAt: time.Now().Add(10 * time.Minute), Interval: 5}, nil
	}
	runtime.donations.poll = func(context.Context, *ustc.DeviceLogin) (ustc.Account, bool, error) {
		return donationAccount(), true, nil
	}
	runtime.donations.verify = func(_ context.Context, account ustc.Account) (ustc.Account, error) { return account, nil }
	return runtime, store
}

type donationBrowser struct {
	testing      *testing.T
	runtime      *Runtime
	cookie       string
	csrf         string
	confirmation string
}

func newDonationBrowser(t *testing.T, runtime *Runtime) *donationBrowser {
	t.Helper()
	browser := &donationBrowser{testing: t, runtime: runtime}
	response := browser.request("session", nil)
	if response.StatusCode != 200 {
		t.Fatal("session creation failed", string(response.Body))
	}
	cookies := (&http.Response{Header: response.Headers}).Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Domain != "" {
		t.Fatal("unsafe session cookie", cookies)
	}
	browser.cookie = cookies[0].Name + "=" + cookies[0].Value
	var body map[string]string
	_ = json.Unmarshal(response.Body, &body)
	browser.csrf = body["csrf"]
	return browser
}

func (browser *donationBrowser) request(operation string, extra http.Header) managementResponse {
	browser.testing.Helper()
	headers := http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"same-origin"}, "X-Ustc-Donation-Origin": {"https://cpa.example"}, "X-Ustc-Donation-Csrf": {browser.csrf}, "Cookie": {browser.cookie}}
	for name, values := range extra {
		headers[http.CanonicalHeaderKey(name)] = values
	}
	service, ctx, _ := browser.runtime.current()
	response := browser.runtime.handleManagement(ctx, service, rpcRequest{Method: "POST", Path: donationManagementPrefix + operation, Headers: headers, HostCallbackID: "donation-scope"})
	for _, secret := range []string{"access-private-secret", "refresh-private-secret", "donor-subject", "ustc-donation-donor", "secret host error"} {
		if strings.Contains(string(response.Body), secret) {
			browser.testing.Errorf("public response leaked %s", secret)
		}
	}
	if response.Headers.Get("Cache-Control") != "no-store" {
		browser.testing.Error("donation API response is cacheable")
	}
	return response
}

func (browser *donationBrowser) authorize(action string) managementResponse {
	browser.testing.Helper()
	response := browser.request("start", http.Header{"X-Ustc-Donation-Action": {action}, "X-Ustc-Donation-Consent": {"share"}})
	if response.StatusCode != 200 {
		browser.testing.Fatal("start failed", string(response.Body))
	}
	response = browser.request("poll", nil)
	var body map[string]any
	_ = json.Unmarshal(response.Body, &body)
	if body["status"] != "ready" {
		browser.testing.Fatal("authorization not ready", string(response.Body))
	}
	browser.confirmation, _ = body["confirmation"].(string)
	return response
}

func (browser *donationBrowser) confirm() managementResponse {
	return browser.request("confirm", http.Header{"X-Ustc-Donation-Confirm": {browser.confirmation}})
}

func TestDonationOptInNativePoolAndWithdrawal(t *testing.T) {
	runtime, store := donatedRuntime(t)
	browser := newDonationBrowser(t, runtime)
	browser.authorize("donate")
	if store.writes != 0 {
		t.Fatal("login implicitly donated without confirmation")
	}
	if response := browser.confirm(); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	name := "ustc-donation-donor.json"
	var saved ustc.Account
	_ = json.Unmarshal(store.files[name], &saved)
	if !saved.Donated || saved.ID != "ustc-donation-donor" || saved.Validate() != nil || saved.RefreshToken == "" {
		t.Fatal("not stored as a native CPA credential")
	}
	if response := browser.confirm(); response.StatusCode != 409 || store.writes != 1 {
		t.Fatal("confirmation replayed")
	}
	saved.CPAFields = map[string]json.RawMessage{"disabled": json.RawMessage("true"), "priority": json.RawMessage("9")}
	store.files[name], _ = json.Marshal(saved)
	duplicate := newDonationBrowser(t, runtime)
	duplicate.authorize("donate")
	if response := duplicate.confirm(); response.StatusCode != 200 || store.writes != 1 {
		t.Fatal("duplicate donation overwrote administrator settings")
	}
	withdraw := newDonationBrowser(t, runtime)
	withdraw.authorize("withdraw")
	if response := withdraw.confirm(); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	_ = json.Unmarshal(store.files[name], &saved)
	if store.writes != 2 || !saved.DonationRevoked || saved.AccessToken != "" || saved.RefreshToken != "" || string(saved.CPAFields["disabled"]) != "true" || string(saved.CPAFields["priority"]) != "9" || saved.Validate() != nil {
		t.Fatal("withdrawal did not preserve a disabled, secret-free tombstone")
	}
	raw := string(store.files[name])
	if strings.Contains(raw, "private-secret") {
		t.Fatal("withdrawal retained school credentials")
	}
	returning := newDonationBrowser(t, runtime)
	returning.authorize("donate")
	if response := returning.confirm(); response.StatusCode != 409 || store.writes != 2 {
		t.Fatal("donor re-enabled a withdrawn record")
	}
}

func TestDonationSameOriginSessionAndConfirmationBoundaries(t *testing.T) {
	runtime, store := donatedRuntime(t)
	browser := newDonationBrowser(t, runtime)
	for _, headers := range []http.Header{
		{"Sec-Fetch-Site": {"cross-site"}}, {"Sec-Fetch-Site": {""}}, {"Origin": {"https://evil.example"}},
		{"X-Ustc-Donation-Origin": {"https://evil.example"}}, {"Sec-Fetch-Mode": {"navigate"}}, {"X-Ustc-Donation-Csrf": {"wrong"}},
	} {
		if response := browser.request("start", headers); response.StatusCode != 403 {
			t.Fatal("cross-site or invalid session accepted", headers, response.StatusCode)
		}
	}
	if response := browser.request("start", http.Header{"X-Ustc-Donation-Action": {"donate"}}); response.StatusCode != 400 {
		t.Fatal("donation consent was not required")
	}
	if response := browser.confirm(); response.StatusCode != 409 {
		t.Fatal("unverified identity could mutate pool")
	}
	other := newDonationBrowser(t, runtime)
	browser.authorize("donate")
	if response := other.request("confirm", http.Header{"X-Ustc-Donation-Confirm": {browser.confirmation}}); response.StatusCode != 409 {
		t.Fatal("cross-session confirmation accepted")
	}
	if response := browser.request("confirm", http.Header{"X-Ustc-Donation-Confirm": {"wrong"}}); response.StatusCode != 409 {
		t.Fatal("missing fresh confirmation accepted")
	}
	if response := browser.confirm(); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	oldConfirmation := browser.confirmation
	browser.authorize("withdraw")
	if response := browser.request("confirm", http.Header{"X-Ustc-Donation-Confirm": {oldConfirmation}}); response.StatusCode != 409 || store.writes != 1 {
		t.Fatal("old flow confirmation replayed against a new identity grant")
	}
}

func TestDonationCannotModifyPrivateOrOtherIdentity(t *testing.T) {
	for _, variant := range []string{"private", "unmarked", "other-subject"} {
		t.Run(variant, func(t *testing.T) {
			runtime, store := donatedRuntime(t)
			account := donationAccount()
			name := account.ID + ".json"
			if variant != "private" {
				account.ID = "ustc-donation-donor"
				name = account.ID + ".json"
			}
			if variant == "other-subject" {
				account.Donated = true
				account.Subject = "somebody-else"
			}
			store.files[name], _ = json.Marshal(account)
			original := string(store.files[name])
			browser := newDonationBrowser(t, runtime)
			browser.authorize("donate")
			if response := browser.confirm(); response.StatusCode != 409 || store.writes != 0 {
				t.Fatal("private or foreign credential could be overwritten")
			}
			withdraw := newDonationBrowser(t, runtime)
			withdraw.authorize("withdraw")
			response := withdraw.confirm()
			if variant != "private" && response.StatusCode != 409 {
				t.Fatal("foreign credential could be withdrawn")
			}
			if store.writes != 0 || string(store.files[name]) != original {
				t.Fatal("non-donation record modified")
			}
		})
	}
}

func TestDonationFailureAndConcurrentConfirmation(t *testing.T) {
	for _, variant := range []string{"bootstrap-denied", "save-error", "simultaneous"} {
		t.Run(variant, func(t *testing.T) {
			runtime, store := donatedRuntime(t)
			if variant == "bootstrap-denied" {
				runtime.donations.verify = func(context.Context, ustc.Account) (ustc.Account, error) {
					return ustc.Account{}, errors.New("private upstream details")
				}
			}
			if variant == "save-error" {
				store.failSave = true
			}
			browser := newDonationBrowser(t, runtime)
			browser.authorize("donate")
			if variant != "simultaneous" {
				if response := browser.confirm(); response.StatusCode != 409 || store.writes != 0 {
					t.Fatal("failed donation committed")
				}
				if response := browser.confirm(); response.StatusCode != 409 {
					t.Fatal("failed save confirmation replayed")
				}
				return
			}
			var success atomic.Int32
			var workers sync.WaitGroup
			for range 8 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					if browser.confirm().StatusCode == 200 {
						success.Add(1)
					}
				}()
			}
			workers.Wait()
			if success.Load() != 1 || store.writes != 1 {
				t.Fatal("concurrent confirmation mutated pool more than once")
			}
		})
	}
}

func TestDonationDisabledExpiryLimitsAndLifecycle(t *testing.T) {
	disabled := configuredRuntime(t)
	for _, name := range []string{"donate", "donate.js", "donate/session", "donate/start", "donate/poll", "donate/confirm"} {
		if callWeb(t, disabled, "GET", resourcePrefix+name, "").StatusCode != 404 {
			t.Fatal("public donation route enabled by default", name)
		}
	}
	runtime, store := donatedRuntime(t)
	now := time.Now()
	for _, name := range []string{"donate", "donate.js", "donate/session", "donate/start", "donate/poll", "donate/confirm"} {
		for _, method := range []string{"GET", "POST"} {
			if callWeb(t, runtime, method, resourcePrefix+name, "").StatusCode != 404 {
				t.Fatal("dynamic resource route remains exposed", method, name)
			}
		}
	}
	if store.calls != 0 {
		t.Fatal("passive resource request invoked privileged host callbacks")
	}
	runtime.donations.now = func() time.Time { return now }
	browser := newDonationBrowser(t, runtime)
	browser.authorize("donate")
	now = now.Add(10 * time.Minute)
	if browser.confirm().StatusCode < 400 || store.writes != 0 {
		t.Fatal("expired grant could commit")
	}
	for range 64 {
		_ = newDonationBrowser(t, runtime)
	}
	response := (&donationBrowser{testing: t, runtime: runtime}).request("session", nil)
	if response.StatusCode != 429 {
		t.Fatal("unbounded anonymous session allocation")
	}
	runtime.donations.close()
	for index := range 7 {
		browser := newDonationBrowser(t, runtime)
		response := browser.request("start", http.Header{"X-Ustc-Donation-Action": {"withdraw"}})
		if (index < 6 && response.StatusCode != 200) || (index == 6 && response.StatusCode != 429) {
			t.Fatal("device authorization rate limit failed", index, response.StatusCode)
		}
	}
	oldPortal := runtime.donations
	responseRPC := invoke(t, runtime, "plugin.reconfigure", map[string]any{"config_yaml": []byte("network: direct\nstate-dir: " + t.TempDir() + "\n")})
	if responseRPC["ok"] != true || len(oldPortal.sessions) != 0 {
		t.Fatal("reconfiguration retained donation credentials or sessions")
	}
}

func TestDonationPoolLimitAndExpiredSessionLockOrdering(t *testing.T) {
	runtime, store := donatedRuntime(t)
	for index := range 256 {
		name := fmt.Sprintf("ustc-donation-limit-%d.json", index)
		store.files[name] = json.RawMessage(`{}`)
	}
	browser := newDonationBrowser(t, runtime)
	browser.authorize("donate")
	if response := browser.confirm(); response.StatusCode != 409 || store.writes != 0 {
		t.Fatal("donation record limit not enforced")
	}
	portal := runtime.donations
	portal.mu.Lock()
	var expired *donationSession
	for _, session := range portal.sessions {
		expired = session
		break
	}
	portal.mu.Unlock()
	expired.expires = time.Now().Add(-time.Minute)
	expired.mu.Lock()
	finished := make(chan managementResponse, 1)
	go func() {
		service, ctx, _ := runtime.current()
		finished <- runtime.handleManagement(ctx, service, rpcRequest{
			Method: "POST", Path: donationManagementPrefix + "session",
			Headers: http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"same-origin"}, "X-Ustc-Donation-Origin": {"https://cpa.example"}},
		})
	}()
	select {
	case response := <-finished:
		expired.mu.Unlock()
		if response.StatusCode != 200 {
			t.Fatal("new session creation failed", string(response.Body))
		}
	case <-time.After(3 * time.Second):
		expired.mu.Unlock()
		<-finished
		t.Fatal("expired-session cleanup deadlocked against an active session")
	}
}
