package ustc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPIKeyExecutionAndStreaming(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer test-api-key" || request.Header.Get("Cookie") != "" {
			t.Error("wrong credential or forwarded client cookie")
		}
		if request.URL.Path == "/v1/models" {
			_, _ = io.WriteString(writer, `{"data":[{"id":"model-one"}]}`)
			return
		}
		if request.URL.Path != "/v1/chat/completions" {
			t.Error("wrong upstream path")
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "upstream-model" {
			t.Error("host-selected model not applied")
		}
		if !body.Stream {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[]}\n\n")
		writer.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	config := DefaultConfig()
	config.Network = "direct"
	config.APIBase = server.URL + "/v1"
	service := NewService(config)
	defer service.Close()
	service.apiClient = func(string) *http.Client { return server.Client() }
	account, err := APIAccount("test-api-key", "", config)
	if err != nil {
		t.Fatal(err)
	}
	models, _, err := service.Models(context.Background(), account)
	if err != nil || len(models) != 1 {
		t.Fatalf("models: %v, %v", models, err)
	}
	response, _, err := service.Execute(context.Background(), account, "upstream-model", []byte(`{"model":"alias","messages":[]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !strings.Contains(string(body), "ok") {
		t.Fatalf("response: %s, %v", body, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, _, err = service.Execute(ctx, account, "upstream-model", []byte(`{"messages":[]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data:") {
		t.Fatalf("first SSE event: %q, %v", line, err)
	}
	cancel()
	_, err = io.ReadAll(reader)
	response.Body.Close()
	if err == nil {
		t.Fatal("cancelled stream did not close")
	}
	if calls.Load() != 3 {
		t.Fatalf("unexpected retries: %d", calls.Load())
	}
}

func TestExecutionRedactsErrorsAndDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{302, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Location", "https://attacker.invalid/collect")
				writer.Header().Set("Retry-After", "7")
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, "credential-secret should never be logged")
			}))
			defer server.Close()
			config := DefaultConfig()
			config.Network = "direct"
			config.APIBase = server.URL + "/v1"
			service := NewService(config)
			defer service.Close()
			service.apiClient = func(string) *http.Client {
				client := server.Client()
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				return client
			}
			account, _ := APIAccount("credential-secret", "", config)
			_, _, err := service.Execute(context.Background(), account, "model", []byte(`{"messages":[]}`), false)
			var upstream *StatusError
			if !errors.As(err, &upstream) || upstream.Status != status || upstream.RetryAfter != 7 {
				t.Fatalf("status lost: %v", err)
			}
			if strings.Contains(err.Error(), "credential-secret") {
				t.Fatal("credential leaked")
			}
			if calls.Load() != 1 {
				t.Fatal("POST unexpectedly retried")
			}
		})
	}
}

func TestEnvironmentKeyIsNotPersisted(t *testing.T) {
	t.Setenv("USTC_TEST_API_KEY", "sensitive-key")
	config := DefaultConfig()
	service := NewService(config)
	defer service.Close()
	account, err := APIAccount("", "USTC_TEST_API_KEY", config)
	if err != nil {
		t.Fatal(err)
	}
	gateway, _, err := service.Resolve(context.Background(), account)
	if err != nil || gateway.Key != "sensitive-key" {
		t.Fatalf("resolve: %v", err)
	}
	data, _ := json.Marshal(account)
	if strings.Contains(string(data), "sensitive-key") {
		t.Fatal("environment credential stored on disk")
	}
	account.ID = "../../overwrite"
	if account.Validate() == nil {
		t.Fatal("path traversal account ID accepted")
	}
}
