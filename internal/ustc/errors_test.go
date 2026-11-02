package ustc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestRetryAfterSeconds(testContext *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 500000000, time.UTC)
	for _, test := range []struct {
		value string
		want  int
	}{
		{"7", 7}, {" 12 ", 12}, {"0", 0}, {"-3", 0}, {"bad", 0}, {"2.5", 0},
		{now.Add(7500 * time.Millisecond).Format(http.TimeFormat), 8},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"9223372036854775807", int(maximumRetrySeconds)},
	} {
		if got := retryAfterSeconds(test.value, now); got != test.want {
			testContext.Errorf("retryAfterSeconds(%q) = %d, want %d", test.value, got, test.want)
		}
	}
}

func TestInferenceErrorClassificationAndRedaction(testContext *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		status  int
		retry   int
	}{
		{"success", `{"choices":[]}`, 0, 0},
		{"null", `{"error":null}`, 0, 0},
		{"false", `{"error":false}`, 0, 0},
		{"invalid", `not JSON`, 0, 0},
		{"quota code", `{"error":{"code":"insufficient_quota","message":"credential-secret","retry_after":2.1}}`, 429, 3},
		{"numeric status", `{"error":{"status":429,"retry_after_seconds":60,"message":"credential-secret"}}`, 429, 60},
		{"numeric code", `{"error":{"code":503}}`, 503, 7},
		{"string status", `{"error":{"code":"429"},"retry_after":20}`, 429, 20},
		{"error type", `{"error":{"type":"rate_limit_exceeded"}}`, 429, 7},
		{"explicit status wins", `{"error":{"status":502,"code":"rate_limit_exceeded"}}`, 502, 7},
		{"authentication", `{"error":{"code":"invalid_api_key"}}`, 401, 7},
		{"unknown", `{"error":{"message":"credential-secret"}}`, 502, 7},
		{"error string", `{"error":"credential-secret"}`, 502, 7},
	} {
		testContext.Run(test.name, func(testContext *testing.T) {
			upstream := InferenceError([]byte(test.payload), http.Header{"Retry-After": {"7"}})
			if test.status == 0 {
				if upstream != nil {
					testContext.Fatalf("successful payload classified as an error: %v", upstream)
				}
				return
			}
			if upstream == nil || upstream.Status != test.status || upstream.RetryAfter != max(test.retry, 7) {
				testContext.Fatalf("classification lost: %#v", upstream)
			}
			if strings.Contains(upstream.Error(), "credential-secret") {
				testContext.Fatal("upstream message leaked")
			}
		})
	}
}

func TestServiceErrorsKeepDateRetryHints(testContext *testing.T) {
	deadline := time.Now().Add(45 * time.Second).UTC().Truncate(time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", deadline.Format(http.TimeFormat))
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, "credential-secret")
	}))
	defer server.Close()
	config := DefaultConfig()
	config.Network = "direct"
	config.APIBase = server.URL + "/v1"
	service := NewService(config)
	defer service.Close()
	service.identity = server.Client()
	service.bootstrapURL = server.URL + "/bootstrap"
	service.apiClient = func(string) *http.Client { return server.Client() }
	account, err := APIAccount("credential-secret", "", config)
	if err != nil {
		testContext.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"inference": func() error {
			_, _, err := service.Execute(context.Background(), account, "model", []byte(`{"messages":[]}`), false)
			return err
		},
		"models": func() error {
			_, _, err := service.Models(context.Background(), account)
			return err
		},
		"bootstrap": func() error {
			_, err := service.bootstrap(context.Background(), Account{AccessToken: "credential-secret"})
			return err
		},
	} {
		testContext.Run(name, func(testContext *testing.T) {
			var upstream *StatusError
			if err := operation(); !errors.As(err, &upstream) || upstream.Status != 429 || upstream.RetryAfter < 40 || upstream.RetryAfter > 45 || strings.Contains(err.Error(), "credential-secret") {
				testContext.Fatalf("date retry hint or safe status lost: %v", err)
			}
		})
	}
}

func TestRelaySSEPreservesEventsAndReportsErrors(testContext *testing.T) {
	normal := ": heartbeat\r\n\r\ndata: {\"choices\":[]}\r\n\r\n"
	for _, test := range []struct {
		name  string
		event string
	}{
		{"envelope", "data: {\"error\":{\"code\":\"rate_limit_exceeded\",\"retry_after\":31,\"message\":\"credential-secret\"}}\n\n"},
		{"named event", "event: error\ndata: {\"status\":429,\"retry_after\":31,\"message\":\"credential-secret\"}\n\n"},
		{"unterminated event", "data: {\"error\":{\"code\":429,\"retry_after\":31}}"},
	} {
		testContext.Run(test.name, func(testContext *testing.T) {
			var emitted strings.Builder
			err := RelaySSE(iotest.OneByteReader(strings.NewReader(normal+test.event)), nil, func(payload []byte) error {
				_, err := emitted.Write(payload)
				return err
			})
			var upstream *StatusError
			if !errors.As(err, &upstream) || upstream.Status != 429 || upstream.RetryAfter != 31 {
				testContext.Fatalf("SSE error lost: %v", err)
			}
			if emitted.String() != normal || strings.Contains(err.Error(), "credential-secret") {
				testContext.Fatalf("error event leaked or prior events changed: %q, %v", emitted.String(), err)
			}
		})
	}
	var emitted strings.Builder
	complete := normal + "data: [DONE]\n\n"
	if err := RelaySSE(strings.NewReader(complete), nil, func(payload []byte) error { _, err := emitted.Write(payload); return err }); err != nil || emitted.String() != complete {
		testContext.Fatalf("successful SSE changed: %q, %v", emitted.String(), err)
	}
	if err := RelaySSE(strings.NewReader(strings.Repeat("x", maximumSSEEventBytes+1)), nil, func([]byte) error { testContext.Fatal("oversized event emitted"); return nil }); err == nil {
		testContext.Fatal("oversized event accepted")
	}
	if err := RelaySSE(iotest.ErrReader(io.ErrUnexpectedEOF), nil, func([]byte) error { return nil }); err == nil {
		testContext.Fatal("truncated stream accepted")
	}
	emitError := errors.New("host stopped")
	if err := RelaySSE(strings.NewReader(normal), nil, func([]byte) error { return emitError }); !errors.Is(err, emitError) {
		testContext.Fatalf("host cancellation lost: %v", err)
	}
}
