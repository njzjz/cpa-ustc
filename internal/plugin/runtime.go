// Package plugin implements the CLIProxyAPI C ABI's JSON RPC contract.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cpa-ustc/internal/ustc"
	"gopkg.in/yaml.v3"
)

// HostCall invokes a host capability and decodes its unwrapped result.
type HostCall func(method string, request any, response any) error

// Repository identifies the plugin source and can be overridden when building a fork.
var Repository = "https://github.com/njzjz/cpa-ustc"

type rpcRequest struct {
	ConfigYAML     []byte `json:"config_yaml"`
	SchemaVersion  uint32 `json:"schema_version"`
	Provider       string
	FileName       string
	RawJSON        []byte
	StorageJSON    []byte
	State          string
	Model          string
	Format         string
	Payload        []byte
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id"`
	Method         string
	Path           string
	Headers        http.Header
	Body           []byte
}

// Runtime owns provider state, concurrent SSO attempts and background SSE relays.
type Runtime struct {
	gate    sync.RWMutex
	mu      sync.Mutex
	service *ustc.Service
	ctx     context.Context
	cancel  context.CancelFunc
	logins  map[string]*ustc.Login
	streams sync.WaitGroup
	Host    HostCall
	webJob  *webJob
}

func New(host HostCall) *Runtime {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{ctx: ctx, cancel: cancel, logins: map[string]*ustc.Login{}, Host: host}
}

func (runtime *Runtime) current() (*ustc.Service, context.Context, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.service == nil {
		return nil, nil, errors.New("plugin has not been configured")
	}
	return runtime.service, runtime.ctx, nil
}

// Close cancels live requests before unloading callbacks or stopping iWAN.
func (runtime *Runtime) Close() {
	// Cancel before waiting for in-flight dispatches to release their read lock.
	runtime.mu.Lock()
	runtime.cancel()
	runtime.mu.Unlock()
	runtime.gate.Lock()
	defer runtime.gate.Unlock()
	runtime.close()
}

func (runtime *Runtime) close() {
	runtime.mu.Lock()
	runtime.cancel()
	service := runtime.service
	runtime.service = nil
	if runtime.webJob != nil {
		runtime.webJob.cancel()
	}
	for state, login := range runtime.logins {
		login.Close()
		delete(runtime.logins, state)
	}
	runtime.mu.Unlock()
	runtime.streams.Wait()
	if service != nil {
		service.Close()
	}
}

func (runtime *Runtime) configure(request rpcRequest) (any, error) {
	config, err := ustc.LoadConfig("")
	if err != nil {
		return nil, err
	}
	if len(request.ConfigYAML) > 0 {
		if err = yaml.Unmarshal(request.ConfigYAML, &config); err != nil {
			return nil, errors.New("invalid plugin configuration YAML")
		}
	}
	if err = config.Validate(); err != nil {
		return nil, err
	}
	runtime.mu.Lock()
	unchanged := runtime.service != nil && runtime.service.Config == config
	runtime.mu.Unlock()
	if !unchanged {
		runtime.close()
		runtime.mu.Lock()
		runtime.ctx, runtime.cancel = context.WithCancel(context.Background())
		runtime.service = ustc.NewService(config)
		runtime.mu.Unlock()
	}
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{"Name": "ustc", "Version": ustc.Version, "Author": "cpa-ustc contributors", "GitHubRepository": Repository, "ConfigFields": []map[string]any{
			{"Name": "network", "Type": "enum", "Default": "auto", "EnumValues": []string{"auto", "direct", "iwan"}, "Description": "校园网直连，校外自动启动 iWAN"},
			{"Name": "iwan-binary", "Type": "string", "Default": "iwan-client-oidc", "Description": "iWAN OIDC 客户端可执行文件"},
			{"Name": "iwan-server", "Type": "string", "Default": "1", "Description": "iWAN 线路序号或名称"},
			{"Name": "state-dir", "Type": "string", "Default": config.StateDir, "Description": "CLI 和插件共享的 iWAN 配置目录"},
			{"Name": "proxy-url", "Type": "string", "Default": "", "Description": "显式使用已有 HTTP(S)/SOCKS5 代理，优先于自动检测"},
			{"Name": "api-base", "Type": "string", "Default": ustc.APIBase, "Description": "直接 API Key 账号的 HTTPS API 地址；不影响学校 SSO 下发的网关"},
		}},
		"capabilities": map[string]any{"auth_provider": true, "model_provider": true, "management_api": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"openai"}, "executor_output_formats": []string{"openai"}},
	}, nil
}

func authData(account ustc.Account) map[string]any {
	data, _ := json.Marshal(account)
	metadata := map[string]any{"type": ustc.Provider}
	for name, raw := range account.CPAFields {
		var value any
		if json.Unmarshal(raw, &value) == nil {
			metadata[name] = value
		}
	}
	disabled, _ := metadata["disabled"].(bool)
	prefix, _ := metadata["prefix"].(string)
	return map[string]any{"Provider": ustc.Provider, "ID": account.ID, "FileName": account.ID + ".json", "Label": account.Label, "Disabled": disabled, "Prefix": prefix, "StorageJSON": data, "Metadata": metadata, "NextRefreshAfter": ustc.RefreshAfter(account)}
}

func decodeAccount(data []byte) (ustc.Account, error) {
	var account ustc.Account
	if json.Unmarshal(data, &account) != nil {
		return account, errors.New("invalid account JSON")
	}
	return account, account.Validate()
}

// Dispatch returns a complete ABI envelope. Errors never embed upstream bodies or tokens.
func (runtime *Runtime) Dispatch(method string, data []byte) []byte {
	if method == "plugin.register" || method == "plugin.reconfigure" {
		runtime.gate.Lock()
		defer runtime.gate.Unlock()
	} else {
		runtime.gate.RLock()
		defer runtime.gate.RUnlock()
	}
	var request rpcRequest
	if len(data) > 0 && json.Unmarshal(data, &request) != nil {
		return failure(errors.New("invalid RPC JSON"))
	}
	result, err := runtime.dispatch(method, request)
	if err != nil {
		return failure(err)
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		return failure(err)
	}
	return raw
}

func failure(err error) []byte {
	status := 500
	retry := 0
	var upstream *ustc.StatusError
	if errors.As(err, &upstream) {
		status = upstream.Status
		retry = upstream.RetryAfter
	}
	raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": "ustc_error", "message": err.Error(), "http_status": status, "retry_after_seconds": retry, "retryable": status == 429 || status >= 500}})
	return raw
}

func (runtime *Runtime) dispatch(method string, request rpcRequest) (any, error) {
	if method == "plugin.register" || method == "plugin.reconfigure" {
		return runtime.configure(request)
	}
	if method == "auth.identifier" || method == "executor.identifier" {
		return map[string]string{"identifier": ustc.Provider}, nil
	}
	if method == "management.register" {
		return managementRegistration(), nil
	}
	service, ctx, err := runtime.current()
	if err != nil {
		return nil, err
	}
	switch method {
	case "management.handle":
		return runtime.handleManagement(ctx, service, request), nil
	case "auth.parse":
		var record struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(request.RawJSON, &record) != nil || record.Type != ustc.Provider {
			return map[string]bool{"Handled": false}, nil
		}
		account, parseErr := decodeAccount(request.RawJSON)
		if parseErr != nil {
			return nil, parseErr
		}
		return map[string]any{"Handled": true, "Auth": authData(account)}, nil
	case "auth.login.start":
		if request.Provider != "" && request.Provider != ustc.Provider {
			return nil, errors.New("unsupported login provider")
		}
		runtime.mu.Lock()
		for state, login := range runtime.logins {
			if !time.Now().Before(login.ExpiresAt) {
				login.Close()
				delete(runtime.logins, state)
			}
		}
		full := len(runtime.logins) >= 8
		runtime.mu.Unlock()
		if full {
			return nil, errors.New("登录尝试过多，请等待已有登录完成")
		}
		login, loginErr := service.StartLogin(ctx, 0)
		if loginErr != nil {
			return nil, loginErr
		}
		runtime.mu.Lock()
		runtime.logins[login.State] = login
		runtime.mu.Unlock()
		return map[string]any{"Provider": ustc.Provider, "URL": login.URL, "State": login.State, "ExpiresAt": login.ExpiresAt}, nil
	case "auth.login.poll":
		runtime.mu.Lock()
		login := runtime.logins[request.State]
		runtime.mu.Unlock()
		if login == nil {
			return nil, errors.New("未知或已过期的登录状态")
		}
		account, done, pollErr := login.Poll()
		if !done {
			return map[string]string{"Status": "pending"}, nil
		}
		if pollErr != nil {
			return map[string]string{"Status": "error", "Message": pollErr.Error()}, nil
		}
		return map[string]any{"Status": "success", "Auth": authData(account)}, nil
	case "model.static":
		return map[string]any{"Provider": ustc.Provider, "Models": []any{}}, nil
	case "model.for_auth":
		account, parseErr := decodeAccount(request.StorageJSON)
		if parseErr != nil {
			return nil, parseErr
		}
		models, updated, modelErr := service.Models(ctx, account)
		if modelErr != nil {
			if persistErr := runtime.persistRotated(account, updated); persistErr != nil {
				return nil, persistErr
			}
			return nil, modelErr
		}
		catalogue := make([]map[string]any, 0, len(models))
		for _, model := range models {
			catalogue = append(catalogue, map[string]any{"ID": model.ID, "Name": model.ID, "Object": "model", "OwnedBy": "ustc", "DisplayName": model.Name, "Type": "openai", "InputTokenLimit": model.Context, "OutputTokenLimit": model.Output})
		}
		return map[string]any{"Provider": ustc.Provider, "Models": catalogue, "AuthUpdate": authData(updated)}, nil
	case "auth.refresh":
		account, parseErr := decodeAccount(request.StorageJSON)
		if parseErr != nil {
			return nil, parseErr
		}
		_, updated, refreshErr := service.Resolve(ctx, account)
		if refreshErr != nil {
			if persistErr := runtime.persistRotated(account, updated); persistErr != nil {
				return nil, persistErr
			}
			return nil, refreshErr
		}
		return map[string]any{"Auth": authData(updated), "NextRefreshAfter": ustc.RefreshAfter(updated)}, nil
	case "executor.execute", "executor.execute_stream":
		return runtime.execute(ctx, service, request, method == "executor.execute_stream")
	case "executor.count_tokens", "executor.http_request":
		return nil, &ustc.StatusError{Status: 501, Message: "学校未公布该操作接口，本插件不估算词元数或转发任意 URL"}
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func (runtime *Runtime) persistRotated(previous, updated ustc.Account) error {
	if previous.AccessToken == updated.AccessToken && previous.RefreshToken == updated.RefreshToken && previous.ExpiresAt.Equal(updated.ExpiresAt) {
		return nil
	}
	if runtime.Host == nil {
		return errors.New("host auth persistence is unavailable")
	}
	data, _ := json.Marshal(updated)
	return runtime.Host("host.auth.save", map[string]any{"name": filepath.Base(updated.ID) + ".json", "json": json.RawMessage(data)}, nil)
}

func (runtime *Runtime) execute(parent context.Context, service *ustc.Service, request rpcRequest, stream bool) (any, error) {
	// CPA normalizes the public chat-completions alias to the canonical openai format.
	if request.Format != "openai" && request.Format != "chat-completions" {
		return nil, &ustc.StatusError{Status: 400, Message: "executor requires chat-completions; enable CPA's built-in translation"}
	}
	account, err := decodeAccount(request.StorageJSON)
	if err != nil {
		return nil, err
	}
	if stream && (request.StreamID == "" || runtime.Host == nil) {
		return nil, errors.New("host stream bridge is required")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	response, updated, err := service.Execute(ctx, account, request.Model, request.Payload, stream)
	// Persist rotation even if bootstrap or inference failed after the token exchange.
	if persistErr := runtime.persistRotated(account, updated); persistErr != nil {
		if response != nil {
			response.Body.Close()
		}
		cancel()
		return nil, persistErr
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if !stream {
		defer cancel()
		defer response.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<20+1))
		if readErr != nil || len(body) > 64<<20 {
			return nil, errors.New("学校模型响应读取失败或超过 64 MiB")
		}
		return map[string]any{"Payload": body, "Headers": safeHeaders(response.Header)}, nil
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body.Close()
		cancel()
		return nil, errors.New("学校网关未返回 SSE 流")
	}
	runtime.streams.Add(1)
	go func() {
		defer runtime.streams.Done()
		defer cancel()
		defer response.Body.Close()
		var streamErr error
		defer func() {
			message := ""
			if streamErr != nil {
				message = "学校模型流中断，请检查校园网/iWAN 或重试"
			}
			_ = runtime.Host("host.stream.close", map[string]any{"stream_id": request.StreamID, "error": message}, nil)
		}()
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := response.Body.Read(buffer)
			if count > 0 {
				if emitErr := runtime.Host("host.stream.emit", map[string]any{"stream_id": request.StreamID, "payload": buffer[:count]}, nil); emitErr != nil {
					streamErr = emitErr
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					streamErr = readErr
				}
				return
			}
		}
	}()
	return map[string]any{"headers": safeHeaders(response.Header)}, nil
}

func safeHeaders(headers http.Header) http.Header {
	result := http.Header{}
	for _, key := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		if values := headers.Values(key); len(values) > 0 {
			result[key] = values
		}
	}
	return result
}
