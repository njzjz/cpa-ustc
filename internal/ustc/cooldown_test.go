package ustc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetCooldownDuration(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		hint       int
		want       int
	}{
		{"default without hint", DefaultBudgetCooldown, 0, 1800},
		{"configured without hint", "3h", 0, 10800},
		{"upstream hint before fallback", "3h", 60, 60},
		{"long upstream hint", DefaultBudgetCooldown, 86400, 86400},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig()
			config.BudgetCooldown = test.configured
			service := NewService(config)
			defer service.Close()
			now := time.Now()
			service.now = func() time.Time { return now }
			account, _ := APIAccount("budget-test", "", config)
			upstream := InferenceError([]byte(`{"error":{"code":"422","type":"budget_exceeded"}}`), nil)
			upstream.RetryAfter = test.hint
			var result *StatusError
			if err := service.RecordInferenceError(account, upstream); !errors.As(err, &result) || !result.BudgetExceeded || result.Status != 429 || result.RetryAfter != test.want {
				t.Fatalf("budget deadline not applied: %#v", result)
			}
			now = now.Add(time.Duration(test.want)*time.Second - 500*time.Millisecond)
			if remaining := service.budgetCooldown(account); remaining == nil || remaining.RetryAfter != 1 {
				t.Fatalf("cooldown rounded down before expiry: %#v", remaining)
			}
			now = now.Add(500 * time.Millisecond)
			if service.budgetCooldown(account) != nil {
				t.Fatal("expired budget cooldown still blocks requests")
			}
		})
	}
}

func TestBudgetCooldownConcurrentFailuresDoNotExtendDeadline(t *testing.T) {
	config := DefaultConfig()
	service := NewService(config)
	defer service.Close()
	now := time.Now()
	service.now = func() time.Time { return now }
	account, _ := APIAccount("budget-test", "", config)
	other, _ := APIAccount("healthy-test", "", config)
	upstream := &StatusError{Status: 429, BudgetExceeded: true}
	service.RecordInferenceError(account, upstream)
	now = now.Add(90*time.Second + 500*time.Millisecond)
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			var result *StatusError
			if err := service.RecordInferenceError(account, upstream); !errors.As(err, &result) || result.RetryAfter != 1710 {
				t.Errorf("concurrent failure extended cooldown: %#v", result)
			}
		}()
	}
	workers.Wait()
	if service.budgetCooldown(other) != nil {
		t.Fatal("one credential cooled another credential")
	}
	now = now.Add(1709500 * time.Millisecond)
	if service.budgetCooldown(account) != nil {
		t.Fatal("in-flight failures slid the original deadline")
	}
	var result *StatusError
	if err := service.RecordInferenceError(account, upstream); !errors.As(err, &result) || result.RetryAfter != 1800 {
		t.Fatalf("new budget failure after expiry did not open a fresh window: %#v", result)
	}
}

func TestBudgetCooldownIgnoresOtherFailures(t *testing.T) {
	config := DefaultConfig()
	service := NewService(config)
	defer service.Close()
	account, _ := APIAccount("budget-test", "", config)
	for _, upstream := range []error{nil, context.Canceled, &StatusError{Status: 422}, &StatusError{Status: 429, RetryAfter: 60}, InferenceError([]byte(`{"error":{"status":401,"type":"budget_exceeded"}}`), nil)} {
		if result := service.RecordInferenceError(account, upstream); result != upstream {
			t.Fatalf("non-budget failure changed: %v", result)
		}
		if service.budgetCooldown(account) != nil {
			t.Fatal("non-budget failure cooled the entire credential")
		}
	}
}

func TestBudgetCooldownBlocksCrossModelUpstreamRequests(t *testing.T) {
	for _, discoveryFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "inference first", true: "discovery first"}[discoveryFirst], func(t *testing.T) {
			var budgetCalls, healthyCalls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Header.Get("Authorization") == "Bearer budget-test" {
					budgetCalls.Add(1)
					writer.WriteHeader(422)
					_, _ = io.WriteString(writer, `{"error":{"code":"422","type":"budget_exceeded"}}`)
					return
				}
				healthyCalls.Add(1)
				_, _ = io.WriteString(writer, `{"choices":[]}`)
			}))
			defer server.Close()
			config := DefaultConfig()
			config.Network, config.APIBase = "direct", server.URL+"/v1"
			service := NewService(config)
			defer service.Close()
			now := time.Now()
			service.now = func() time.Time { return now }
			service.apiClient = func(string) *http.Client { return server.Client() }
			account, _ := APIAccount("budget-test", "", config)
			healthy, _ := APIAccount("healthy-test", "", config)
			if discoveryFirst {
				_, _, _ = service.Models(context.Background(), account)
			}
			for _, model := range []string{"model-one", "model-two"} {
				for _, stream := range []bool{false, true} {
					_, _, err := service.Execute(context.Background(), account, model, []byte(`{"messages":[]}`), stream)
					var result *StatusError
					if !errors.As(err, &result) || result.RetryAfter != 1800 || !result.BudgetExceeded {
						t.Fatalf("missing credential-wide cooldown: %v", err)
					}
				}
			}
			_, _, _ = service.Models(context.Background(), account)
			if budgetCalls.Load() != 1 {
				t.Fatalf("budget exhausted credential contacted upstream %d times", budgetCalls.Load())
			}
			response, _, err := service.Execute(context.Background(), healthy, "model-one", []byte(`{"messages":[]}`), false)
			if err != nil {
				t.Fatal("healthy account blocked", err)
			}
			response.Body.Close()
			if healthyCalls.Load() != 1 {
				t.Fatal("healthy account did not reach upstream")
			}
			now = now.Add(30 * time.Minute)
			_, _, _ = service.Execute(context.Background(), account, "model-two", []byte(`{"messages":[]}`), false)
			if budgetCalls.Load() != 2 {
				t.Fatal("expired credential could not retry upstream")
			}
		})
	}
}

func TestBudgetCooldownConfigValidation(t *testing.T) {
	for _, value := range []string{"", "0", "0s", "-1m", "500ms", "169h", "30minutes"} {
		config := DefaultConfig()
		config.BudgetCooldown = value
		if config.Validate() == nil {
			t.Errorf("invalid cooldown accepted: %q", value)
		}
	}
	for _, value := range []string{"1s", "30m", "3h", "168h"} {
		config := DefaultConfig()
		config.BudgetCooldown = value
		if err := config.Validate(); err != nil {
			t.Errorf("valid cooldown rejected: %q: %v", value, err)
		}
	}
}
