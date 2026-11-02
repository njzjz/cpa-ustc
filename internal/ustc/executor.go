package ustc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Execute sends only translated chat-completions payloads and plugin-owned headers.
// Stream bodies remain live; ownership passes to the caller, which must close them.
func (service *Service) Execute(ctx context.Context, account Account, model string, payload []byte, stream bool) (*http.Response, Account, error) {
	if err := account.Validate(); err != nil {
		return nil, account, err
	}
	if cooldown := service.budgetCooldown(account); cooldown != nil {
		return nil, account, cooldown
	}
	gateway, updated, err := service.Resolve(ctx, account)
	if err != nil {
		return nil, updated, err
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil || body == nil {
		return nil, updated, &StatusError{Status: 400, Message: "请求必须为 JSON 对象"}
	}
	if model == "" {
		return nil, updated, &StatusError{Status: 400, Message: "model 不能为空"}
	}
	body["model"], _ = json.Marshal(model)
	body["stream"], _ = json.Marshal(stream)
	data, _ := json.Marshal(body)
	proxy, err := service.Network.Proxy(ctx)
	if err != nil {
		return nil, updated, err
	}
	client := service.apiClient(proxy)
	if cooldown := service.budgetCooldown(account); cooldown != nil {
		client.CloseIdleConnections()
		return nil, updated, cooldown
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway.Base+"/chat/completions", bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+gateway.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "cpa-ustc/"+Version)
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := client.Do(request)
	if err != nil {
		client.CloseIdleConnections()
		return nil, updated, errors.New("学校模型请求失败，请检查校园网/iWAN 或稍后重试")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstream := modelResponseError(response, "学校模型网关拒绝请求")
		response.Body.Close()
		client.CloseIdleConnections()
		return nil, updated, service.RecordInferenceError(account, upstream)
	}
	response.Body = &ownedBody{ReadCloser: response.Body, client: client}
	return response, updated, nil
}

type ownedBody struct {
	io.ReadCloser
	client *http.Client
}

func (body *ownedBody) Close() error {
	err := body.ReadCloser.Close()
	body.client.CloseIdleConnections()
	return err
}

// RefreshAfter gives CPA a conservative refresh time, also for non-OAuth keys.
func RefreshAfter(account Account) time.Time {
	if account.ExpiresAt.IsZero() {
		return time.Now().Add(24 * time.Hour)
	}
	expiry := account.ExpiresAt.Add(-2 * time.Minute)
	if expiry.Before(time.Now()) {
		return time.Now().Add(15 * time.Second)
	}
	return expiry
}
