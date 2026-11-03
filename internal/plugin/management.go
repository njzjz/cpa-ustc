package plugin

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cpa-ustc/internal/setup"
	"cpa-ustc/internal/ustc"
	"gopkg.in/yaml.v3"
)

// CPA resource handlers serve passive assets only. Every operation, including
// donation session creation, goes through the host's management authentication.
//
//go:embed web/*
var webAssets embed.FS

const managementPrefix = "/v0/management/plugins/ustc/"
const resourcePrefix = "/v0/resource/plugins/ustc/"
const donationManagementPrefix = managementPrefix + "donation/"

func managementRegistration() any {
	routes := []map[string]string{}
	for _, route := range []struct{ method, path string }{
		{"GET", "state"}, {"POST", "validate"}, {"GET", "accounts"},
		{"POST", "accounts"}, {"POST", "oauth/callback"},
		{"POST", "iwan/login"}, {"POST", "iwan/install"},
		{"GET", "iwan/job"}, {"POST", "iwan/callback"},
		{"POST", "donation/session"}, {"POST", "donation/start"},
		{"POST", "donation/poll"}, {"POST", "donation/confirm"},
	} {
		routes = append(routes, map[string]string{"Method": route.method, "Path": managementPrefix + route.path})
	}
	return map[string]any{"routes": routes, "resources": []map[string]string{
		{"Path": "/settings", "Menu": "中科大词元计划", "Description": "网络设置、科大登录、API Key 和 CPA 号池"},
		{"Path": "/settings.js"}, {"Path": "/settings.css"},
	}}
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func webResponse(status int, contentType string, data []byte) managementResponse {
	return managementResponse{status, http.Header{
		"Content-Type": {contentType}, "Cache-Control": {"no-store"},
		"X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"},
		"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
	}, data}
}

func webJSON(status int, data any) managementResponse {
	raw, _ := json.Marshal(data)
	return webResponse(status, "application/json; charset=utf-8", raw)
}

func webError(status int, message string) managementResponse {
	return webJSON(status, map[string]string{"error": message})
}

// decodeWebBody bounds inputs, rejects trailing JSON and unknown fields, and
// requires JSON so cross-origin HTML forms cannot trigger mutations.
func decodeWebBody(request rpcRequest, target any) error {
	if len(request.Body) > 64<<10 || !strings.HasPrefix(strings.ToLower(request.Headers.Get("Content-Type")), "application/json") {
		return errors.New("请求必须是大小不超过 64 KiB 的 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("请求 JSON 格式无效")
	}
	return nil
}

func publicConfig(config ustc.Config) map[string]string {
	return map[string]string{"network": config.Network, "proxy-url": config.ProxyURL,
		"state-dir": config.StateDir, "iwan-binary": config.IwanBinary,
		"iwan-server": config.IwanServer, "api-base": config.APIBase, "budget-cooldown": config.BudgetCooldown,
		"donation-mode": config.DonationMode, "donation-origin": config.DonationOrigin}
}

func (runtime *Runtime) handleManagement(ctx context.Context, service *ustc.Service, request rpcRequest) managementResponse {
	if strings.HasPrefix(request.Path, donationManagementPrefix) {
		if request.Method != http.MethodPost || runtime.donations == nil || service.Config.DonationMode != "open" {
			return webError(404, "捐献入口未开放")
		}
		return runtime.donations.handle(ctx, runtime.Host, request)
	}
	if request.Method == http.MethodGet && strings.HasPrefix(request.Path, resourcePrefix) {
		name := strings.TrimPrefix(request.Path, resourcePrefix)
		contentType := ""
		switch name {
		case "settings":
			name, contentType = "settings.html", "text/html; charset=utf-8"
		case "settings.js":
			contentType = "application/javascript; charset=utf-8"
		case "settings.css":
			contentType = "text/css; charset=utf-8"
		default:
			return webError(404, "资源不存在")
		}
		data, _ := webAssets.ReadFile("web/" + name)
		return webResponse(200, contentType, data)
	}
	if !strings.HasPrefix(request.Path, managementPrefix) {
		return webError(404, "接口不存在")
	}
	operation := request.Method + " " + strings.TrimPrefix(request.Path, managementPrefix)
	switch operation {
	case "GET state":
		_, binaryErr := exec.LookPath(service.Config.IwanBinary)
		_, configErr := os.Stat(filepath.Join(service.Config.StateDir, "iwan", "servers.json"))
		return webJSON(200, map[string]any{"config": publicConfig(service.Config), "iwan_installed": binaryErr == nil, "iwan_configured": configErr == nil})
	case "POST validate":
		var values map[string]string
		if err := decodeWebBody(request, &values); err != nil {
			return webError(400, err.Error())
		}
		config := service.Config
		data, _ := yaml.Marshal(values)
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if decoder.Decode(&config) != nil {
			return webError(400, "未知配置字段或字段类型错误")
		}
		if err := config.Validate(); err != nil {
			return webError(400, err.Error())
		}
		return webJSON(200, map[string]string{"status": "ok"})
	case "GET accounts":
		return runtime.webAccounts(request.HostCallbackID)
	case "POST accounts":
		var input struct {
			APIKey    string `json:"api_key"`
			APIKeyEnv string `json:"api_key_env"`
			Label     string `json:"label"`
		}
		if err := decodeWebBody(request, &input); err != nil {
			return webError(400, err.Error())
		}
		if (input.APIKey == "") == (input.APIKeyEnv == "") || len(input.Label) > 256 {
			return webError(400, "请填写 API Key 或环境变量名中的一种；备注不得超过 256 字节")
		}
		account, err := ustc.APIAccount(input.APIKey, input.APIKeyEnv, service.Config)
		if err != nil {
			return webError(400, err.Error())
		}
		if input.Label != "" {
			account.Label = input.Label
		}
		if runtime.Host == nil {
			return webError(503, "CPA 凭据存储不可用")
		}
		data, _ := json.Marshal(account)
		if err = runtime.Host("host.auth.save", map[string]any{"host_callback_id": request.HostCallbackID, "name": account.ID + ".json", "json": json.RawMessage(data)}, nil); err != nil {
			return webError(502, "无法写入 CPA 号池")
		}
		return webJSON(200, map[string]string{"id": account.ID, "name": account.ID + ".json", "label": account.Label})
	case "POST oauth/callback":
		var input struct {
			URL string `json:"url"`
		}
		if err := decodeWebBody(request, &input); err != nil {
			return webError(400, err.Error())
		}
		parsed, err := url.Parse(input.URL)
		if err != nil {
			return webError(400, "回调链接无效")
		}
		runtime.mu.Lock()
		login := runtime.logins[parsed.Query().Get("state")]
		runtime.mu.Unlock()
		if login == nil {
			return webError(400, "登录会话不存在或已过期")
		}
		if err = login.SubmitCallback(ctx, input.URL); err != nil {
			return webError(400, err.Error())
		}
		return webJSON(200, map[string]string{"status": "ok"})
	case "POST iwan/login", "POST iwan/install":
		var input struct{}
		if err := decodeWebBody(request, &input); err != nil {
			return webError(400, err.Error())
		}
		return runtime.startWebJob(ctx, service, strings.HasSuffix(operation, "install"))
	case "GET iwan/job":
		runtime.mu.Lock()
		job := runtime.webJob
		runtime.mu.Unlock()
		if job == nil {
			return webJSON(200, map[string]string{"status": "idle"})
		}
		return webJSON(200, job.snapshot())
	case "POST iwan/callback":
		var input struct {
			URL string `json:"url"`
		}
		if err := decodeWebBody(request, &input); err != nil {
			return webError(400, err.Error())
		}
		runtime.mu.Lock()
		job := runtime.webJob
		runtime.mu.Unlock()
		if job == nil || job.submit(input.URL) != nil {
			return webError(400, "iWAN 回调无效、state 不匹配或会话已结束")
		}
		return webJSON(200, map[string]string{"status": "ok"})
	default:
		return webError(404, "接口不存在或请求方法不支持")
	}
}

// webAccounts projects CPA's credential list, not a separate plugin account pool.
// Neither raw credential JSON nor host-local paths are sent to the browser.
func (runtime *Runtime) webAccounts(callbackID string) managementResponse {
	var result struct {
		Files []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Type           string `json:"type"`
			Provider       string `json:"provider"`
			Label          string `json:"label"`
			Status         string `json:"status"`
			StatusMessage  string `json:"status_message"`
			Unavailable    bool   `json:"unavailable"`
			NextRetryAfter string `json:"next_retry_after,omitempty"`
			Disabled       bool   `json:"disabled"`
			AuthIndex      string `json:"auth_index"`
		} `json:"files"`
	}
	if runtime.Host == nil || runtime.Host("host.auth.list", map[string]any{"host_callback_id": callbackID}, &result) != nil {
		return webError(502, "无法读取 CPA 号池")
	}
	accounts := []any{}
	for _, account := range result.Files {
		if account.Provider == ustc.Provider || account.Type == ustc.Provider {
			accounts = append(accounts, account)
		}
	}
	return webJSON(200, map[string]any{"accounts": accounts})
}

// webJob bridges the upstream interactive helper without returning its logs,
// which may include tokens. It retains only the allowlisted authorization URL.
type webJob struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	input     io.WriteCloser
	status    string
	url       string
	binary    string
	message   string
	buffer    string
	submitted bool
}

func (job *webJob) snapshot() map[string]string {
	job.mu.Lock()
	defer job.mu.Unlock()
	return map[string]string{"status": job.status, "url": job.url, "binary": job.binary, "message": job.message}
}

func (job *webJob) Write(data []byte) (int, error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.buffer += string(data)
	boundary := strings.LastIndexAny(job.buffer, " \t\r\n")
	if boundary < 0 {
		if len(job.buffer) > 16<<10 {
			job.buffer = ""
		}
		return len(data), nil
	}
	for _, field := range strings.Fields(job.buffer[:boundary]) {
		parsed, err := url.Parse(field)
		if err == nil && parsed.Scheme == "https" && parsed.Host == "auth.ivpn.ustc.edu.cn" && parsed.Path == "/login/oauth/authorize" && parsed.User == nil && parsed.Fragment == "" && parsed.Query().Get("state") != "" && parsed.Query().Get("code_challenge") != "" {
			job.url = field
		}
	}
	job.buffer = job.buffer[boundary+1:]
	if len(job.buffer) > 16<<10 {
		job.buffer = job.buffer[len(job.buffer)-(16<<10):]
	}
	return len(data), nil
}

func (job *webJob) submit(raw string) error {
	job.mu.Lock()
	callback, err := url.Parse(raw)
	authorization, _ := url.Parse(job.url)
	if err != nil || authorization == nil || job.url == "" || job.input == nil || job.status != "pending" || job.submitted || len(raw) > 16<<10 || strings.ContainsAny(raw, "\r\n") || callback.Scheme != "com.panabit.mobile" || callback.Host != "oauth2redirect" || callback.User != nil || callback.Fragment != "" || callback.Path != "" {
		job.mu.Unlock()
		return errors.New("invalid callback")
	}
	query, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") != authorization.Query().Get("state") || len(query["code"]) != 1 || query.Get("code") == "" {
		job.mu.Unlock()
		return errors.New("invalid state or code")
	}
	job.submitted = true
	input := job.input
	job.mu.Unlock()
	_, err = io.WriteString(input, raw+"\n")
	_ = input.Close()
	return err
}

func (runtime *Runtime) startWebJob(parent context.Context, service *ustc.Service, install bool) managementResponse {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.webJob != nil && runtime.webJob.snapshot()["status"] == "pending" {
		return webError(409, "已有 iWAN 操作进行中")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	// A real file descriptor avoids exec.Cmd waiting on a blocked stdin-copy
	// goroutine when the helper exits before the browser submits its callback.
	reader, writer, pipeErr := os.Pipe()
	if pipeErr != nil {
		cancel()
		return webError(500, "无法创建 iWAN 登录通道")
	}
	job := &webJob{cancel: cancel, input: writer, status: "pending"}
	runtime.webJob = job
	runtime.streams.Add(1)
	go func() {
		defer runtime.streams.Done()
		defer cancel()
		defer reader.Close()
		defer writer.Close()
		var binary string
		var err error
		if install {
			binary, err = setup.InstallIwan(ctx, service.Config.StateDir)
		} else {
			err = service.Network.Login(ctx, reader, job)
		}
		job.mu.Lock()
		defer job.mu.Unlock()
		job.status, job.binary, job.url, job.buffer = "success", binary, "", ""
		if err != nil {
			job.status = "error"
			job.message = "iWAN 操作失败，请检查客户端路径、网络及授权；可重新发起"
		}
	}()
	return webJSON(202, map[string]string{"status": "pending"})
}
