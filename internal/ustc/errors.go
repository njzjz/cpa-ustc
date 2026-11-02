package ustc

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maximumRetrySeconds = int64((1<<63 - 1) / time.Second)

// retryAfterSeconds accepts both HTTP Retry-After forms and rounds up so that
// converting an HTTP date to CPA's integral seconds never shortens the cooldown.
func retryAfterSeconds(value string, now time.Time) int {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return int(min(seconds, maximumRetrySeconds))
	}
	deadline, err := http.ParseTime(value)
	if err != nil || !deadline.After(now) {
		return 0
	}
	delay := deadline.Sub(now)
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	return int(min(seconds, maximumRetrySeconds))
}

func responseStatusError(response *http.Response, message string) *StatusError {
	if response.StatusCode == http.StatusTooManyRequests {
		message = "学校服务限流或配额暂不可用，请等待 CPA 冷却后重试"
	}
	return &StatusError{Status: response.StatusCode, Message: message, RetryAfter: retryAfterSeconds(response.Header.Get("Retry-After"), time.Now())}
}

func retryJSONSeconds(raw json.RawMessage) int {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return retryAfterSeconds(text, time.Now())
	}
	seconds, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	return int(math.Ceil(math.Min(seconds, float64(maximumRetrySeconds))))
}

// InferenceError recognizes standard error envelopes even when a gateway sends
// HTTP 200. Only status codes and retry hints are retained; upstream messages may
// contain credentials and must never become CPA diagnostics or client output.
func InferenceError(payload []byte, headers http.Header) *StatusError {
	var envelope struct {
		Error      json.RawMessage `json:"error"`
		RetryAfter json.RawMessage `json:"retry_after"`
	}
	if json.Unmarshal(payload, &envelope) != nil || len(envelope.Error) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) || bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("false")) {
		return nil
	}
	var detail struct {
		Code              json.RawMessage `json:"code"`
		Type              string          `json:"type"`
		Status            int             `json:"status"`
		StatusCode        int             `json:"status_code"`
		HTTPStatus        int             `json:"http_status"`
		RetryAfter        json.RawMessage `json:"retry_after"`
		RetryAfterSeconds json.RawMessage `json:"retry_after_seconds"`
	}
	_ = json.Unmarshal(envelope.Error, &detail)
	status := 0
	for _, candidate := range []int{detail.Status, detail.StatusCode, detail.HTTPStatus} {
		if candidate >= 400 && candidate <= 599 {
			status = candidate
			break
		}
	}
	if status == 0 {
		var code string
		if json.Unmarshal(detail.Code, &code) != nil {
			code = string(detail.Code)
		}
		for _, candidate := range []string{code, detail.Type} {
			if numeric, err := strconv.Atoi(candidate); err == nil && numeric >= 400 && numeric <= 599 {
				status = numeric
				break
			}
			switch strings.ToLower(candidate) {
			case "rate_limit_exceeded", "too_many_requests", "insufficient_quota", "quota_exceeded":
				status = http.StatusTooManyRequests
			case "overloaded_error", "service_unavailable":
				status = http.StatusServiceUnavailable
			case "invalid_api_key", "authentication_error":
				status = http.StatusUnauthorized
			case "permission_denied", "permission_error":
				status = http.StatusForbidden
			}
			if status != 0 {
				break
			}
		}
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	message := "学校模型网关返回错误"
	if status == http.StatusTooManyRequests {
		message = "学校模型网关限流或配额暂不可用，请等待 CPA 冷却后重试"
	}
	retry := retryAfterSeconds(headers.Get("Retry-After"), time.Now())
	for _, hint := range []json.RawMessage{detail.RetryAfterSeconds, detail.RetryAfter, envelope.RetryAfter} {
		retry = max(retry, retryJSONSeconds(hint))
	}
	return &StatusError{Status: status, Message: message, RetryAfter: retry}
}
