package ustc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func usageFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestQuotaNormalization(t *testing.T) {
	quota, err := parseQuota(usageFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(quota.Groups) != 2 || len(quota.Groups[0].Buckets) != 2 || len(quota.Summary) != 3 {
		t.Fatalf("lost budget scope or windows: %+v", quota)
	}
	if quota.Groups[1].DisplayName != "用户共享额度" || quota.Summary[2].Value != 94.5 {
		t.Fatalf("shared budget misrepresented: %+v", quota)
	}
	for index, expected := range []float64{28.0 / 30, 65.75 / 70} {
		if math.Abs(quota.Groups[0].Buckets[index].RemainingFraction-expected) > 1e-12 {
			t.Fatal("remaining fraction is not upstream remaining / limit")
		}
	}
	if quota.Groups[0].Buckets[0].ResetTime != "2026-11-02T04:00:00Z" {
		t.Fatal("reset timestamp lost its time zone")
	}
}

func TestQuotaNeverInventsUnknownBalances(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any, map[string]any, map[string]any)
	}{
		{"pending", func(root, key, window map[string]any) { key["error_code"] = "snapshot_pending" }},
		{"stale", func(root, key, window map[string]any) { window["status"] = "stale" }},
		{"stale freshness", func(root, key, window map[string]any) { root["freshness"] = "stale" }},
		{"reset elapsed", func(root, key, window map[string]any) { window["error_code"] = "snapshot_reset_elapsed" }},
		{"null spend", func(root, key, window map[string]any) { window["current_spend"] = nil }},
		{"missing spend", func(root, key, window map[string]any) { delete(window, "current_spend") }},
		{"nan spend", func(root, key, window map[string]any) { window["current_spend"] = "NaN" }},
		{"infinite limit", func(root, key, window map[string]any) { window["max_budget"] = "1e9999" }},
		{"zero limit", func(root, key, window map[string]any) { window["max_budget"] = 0 }},
		{"negative spend", func(root, key, window map[string]any) { window["current_spend"] = -1 }},
		{"disabled", func(root, key, window map[string]any) { key["status"] = "disabled" }},
		{"unavailable", func(root, key, window map[string]any) { root["status"] = "unavailable" }},
		{"unsafe duration", func(root, key, window map[string]any) { window["budget_duration"] = "secret-<script>" }},
		{"unknown shared spend", func(root, key, window map[string]any) { root["account"].(map[string]any)["spend"] = nil }},
		{"too many windows", func(root, key, window map[string]any) {
			windows := make([]any, 65)
			for index := range windows {
				windows[index] = window
			}
			key["budget_windows"] = windows
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(usageFixture(t), &root); err != nil {
				t.Fatal(err)
			}
			key := root["key"].(map[string]any)
			window := key["budget_windows"].([]any)[0].(map[string]any)
			test.edit(root, key, window)
			body, _ := json.Marshal(root)
			quota, err := parseQuota(body)
			if err == nil || len(quota.Groups) != 0 || len(quota.Summary) != 0 {
				t.Fatalf("unknown quota became usable: %+v, %v", quota, err)
			}
		})
	}
	for _, body := range []string{`null`, `{}`, `{"key":null}`, `{"key":{}} trailing`, `{"key":[]}`} {
		if _, err := parseQuota([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestQuotaOverspendAndUnlimitedWindows(t *testing.T) {
	body := strings.Replace(string(usageFixture(t)), `"current_spend": 2`, `"current_spend": 32`, 1)
	quota, err := parseQuota([]byte(body))
	if err != nil || quota.Groups[0].Buckets[0].RemainingFraction != 0 || quota.Summary[0].Value != -2 {
		t.Fatalf("overspend was hidden: %+v, %v", quota, err)
	}
	body = strings.Replace(string(usageFixture(t)), `"limit_status": "limited"`, `"limit_status": "unlimited"`, 1)
	quota, err = parseQuota([]byte(body))
	if err != nil || len(quota.Groups[0].Buckets) != 1 {
		t.Fatalf("unlimited window rendered as zero: %+v, %v", quota, err)
	}
}

func TestQuotaTimestampVariants(t *testing.T) {
	for _, raw := range []string{`1735689600`, `1735689600000`, `"1735689600000"`, `"2025-01-01T08:00:00+08:00"`} {
		if actual := budgetTime(json.RawMessage(raw)); actual != "2025-01-01T00:00:00Z" {
			t.Fatalf("timestamp %s: %s", raw, actual)
		}
	}
	for _, raw := range []string{`null`, `0`, `-1`, `"NaN"`, `"2026-11-02T10:00:00"`, `1e100`} {
		if actual := budgetTime(json.RawMessage(raw)); actual != "" {
			t.Fatalf("guessed timestamp %s: %s", raw, actual)
		}
	}
}

func TestQuotaPartialSnapshotAndScope(t *testing.T) {
	body := strings.Replace(string(usageFixture(t)), `"status": "ok"`, `"status": "partial"`, 1)
	body = strings.Replace(body, `"scope": "user"`, `"scope": "project"`, 1)
	body = strings.Replace(body, `"shared": true`, `"shared": false`, 1)
	quota, err := parseQuota([]byte(body))
	if err != nil || quota.Groups[1].DisplayName != "项目额度" || !strings.Contains(quota.Groups[0].Buckets[0].Description, "未展示部分状态未知") {
		t.Fatalf("partial response or independent budget misrepresented: %+v, %v", quota, err)
	}
}

func TestQuotaAccountIsolationAndCancellation(t *testing.T) {
	service := NewService(DefaultConfig())
	defer service.Close()
	first := oauthAccount()
	first.AccessToken, first.ExpiresAt = "first-access", time.Now().Add(time.Hour)
	second := first
	second.ID, second.AccessToken, second.Subject = "ustc-sso-second", "second-access", "second-subject"
	body := string(usageFixture(t))
	service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		responseBody := body
		switch request.Header.Get("Authorization") {
		case "Bearer first-access":
		case "Bearer second-access":
			responseBody = strings.Replace(body, `"current_spend": 2`, `"current_spend": 12`, 1)
		default:
			t.Error("unexpected account credential")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(responseBody))}, nil
	})
	for _, account := range []Account{first, second, first} {
		quota, updated, err := service.FetchQuota(context.Background(), account)
		expected := 28.0
		if account.ID == second.ID {
			expected = 18
		}
		if err != nil || quota.Summary[0].Value != expected || updated.Subject != account.Subject {
			t.Fatalf("quota crossed account boundaries: %+v, %v", quota, err)
		}
	}
	service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := service.FetchQuota(ctx, first)
	if err == nil {
		t.Fatal("cancelled quota request succeeded")
	}
}

func TestQuotaRefreshSharedWithInference(t *testing.T) {
	fixture := newIdentity(t)
	body := usageFixture(t)
	var queries atomic.Int32
	fixture.service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != UsageURL {
			return fixture.handle(request)
		}
		queries.Add(1)
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer new-access" || request.Header.Get("User-Agent") != "cpa-ustc/"+Version {
			t.Error("quota used a gateway key, stale token or wrong identity endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	account := oauthAccount()
	var workers sync.WaitGroup
	for index := 0; index < 10; index++ {
		workers.Add(1)
		go func(inference bool) {
			defer workers.Done()
			var updated Account
			var err error
			if inference {
				_, updated, err = fixture.service.Resolve(context.Background(), account)
			} else {
				_, updated, err = fixture.service.FetchQuota(context.Background(), account)
			}
			if err != nil || updated.RefreshToken != "rotated-refresh" {
				t.Errorf("rotation lost: %+v, %v", updated, err)
			}
		}(index%2 == 0)
	}
	workers.Wait()
	if queries.Load() != 5 || fixture.refreshes.Load() != 1 || fixture.bootstraps.Load() != 1 {
		t.Fatalf("duplicate rotation or inference used to query quota: %d/%d/%d", queries.Load(), fixture.refreshes.Load(), fixture.bootstraps.Load())
	}
}

func TestQuotaErrorsAreBoundedAndRedacted(t *testing.T) {
	for _, status := range []int{200, 302, 401, 403, 404, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Location", "https://attacker.invalid/token")
				writer.Header().Set("Retry-After", "5")
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, strings.Repeat("secret-access ", 30<<10))
			}))
			defer server.Close()
			service := NewService(DefaultConfig())
			defer service.Close()
			service.usageURL = server.URL
			service.identity = server.Client()
			service.identity.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			account := oauthAccount()
			account.AccessToken, account.ExpiresAt = "secret-access", time.Now().Add(time.Hour)
			_, _, err := service.FetchQuota(context.Background(), account)
			if err == nil || strings.Contains(err.Error(), "secret-access") || calls.Load() != 1 {
				t.Fatalf("unsafe quota error: %v, calls=%d", err, calls.Load())
			}
			if status != 200 {
				var upstream *StatusError
				if !errors.As(err, &upstream) || upstream.Status != status || upstream.RetryAfter != 5 {
					t.Fatalf("lost status: %v", err)
				}
			}
		})
	}
}

func TestQuotaAPIKeyNeverContactsSchool(t *testing.T) {
	service := NewService(DefaultConfig())
	defer service.Close()
	service.identity.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("API key sent to an identity endpoint")
		return nil, nil
	})
	for _, environment := range []string{"", "USTC_TEST_KEY"} {
		account, err := APIAccount("secret-api-key", environment, service.Config)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = service.FetchQuota(context.Background(), account)
		var upstream *StatusError
		if !errors.As(err, &upstream) || upstream.Status != 501 || strings.Contains(err.Error(), "secret-api-key") {
			t.Fatalf("incorrect unsupported response: %v", err)
		}
	}
}

func TestQuotaFailureReturnsRotatedToken(t *testing.T) {
	fixture := newIdentity(t)
	fixture.service.identity.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != UsageURL {
			return fixture.handle(request)
		}
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("new-access must not leak"))}, nil
	})
	_, updated, err := fixture.service.FetchQuota(context.Background(), oauthAccount())
	if err == nil || updated.RefreshToken != "rotated-refresh" || strings.Contains(err.Error(), "new-access") || fixture.bootstraps.Load() != 0 {
		t.Fatalf("quota failure lost rotated token or bootstrapped a model: %v", err)
	}
}
