package ustc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DeviceLogin exposes only the user-facing code. The device code is a bearer
// credential and must never be returned to a browser or serialized to disk.
type DeviceLogin struct {
	VerificationURL string
	UserCode        string
	ExpiresAt       time.Time
	Interval        int
	mu              sync.Mutex
	document        discovery
	code            string
	started         time.Time
	nextPoll        time.Time
}

// Close discards a pending device credential; it does not revoke school tokens.
func (login *DeviceLogin) Close() {
	login.mu.Lock()
	defer login.mu.Unlock()
	login.code = ""
}

// StartDeviceLogin uses the published public client, without a client secret or
// redirect URI. Only explicitly initiated donations may call this operation.
func (service *Service) StartDeviceLogin(ctx context.Context) (*DeviceLogin, error) {
	if err := service.CheckPublishedProfile(ctx); err != nil {
		return nil, err
	}
	document, err := service.discover(ctx)
	if err != nil {
		return nil, err
	}
	if document.Device == "" || !schoolURL(document.Device) || !strings.HasPrefix(document.Device, Issuer+"/") {
		return nil, errors.New("学校尚未提供可信的设备授权接口")
	}
	status, body, err := service.deviceRequest(ctx, document.Device, url.Values{"client_id": {"tokenworks"}, "scope": {"openid profile email offline_access"}})
	if err != nil {
		return nil, err
	}
	var response struct {
		Code     string `json:"device_code"`
		UserCode string `json:"user_code"`
		URL      string `json:"verification_uri"`
		Complete string `json:"verification_uri_complete"`
		Expires  int    `json:"expires_in"`
		Interval int    `json:"interval"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &response) != nil || response.Code == "" || len(response.Code) > 8192 || response.UserCode == "" || response.UserCode == response.Code || len(response.UserCode) > 128 || response.Expires <= 0 || response.Expires > 1800 || response.Interval < 0 || response.Interval > 300 {
		return nil, errors.New("学校设备授权不可用或返回格式无效")
	}
	verificationURL := response.URL
	if response.Complete != "" {
		verificationURL = response.Complete
	}
	parsed, err := url.Parse(verificationURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "id.ustc.edu.cn" || parsed.User != nil || parsed.Fragment != "" || len(verificationURL) > 8192 || strings.Contains(verificationURL, response.Code) {
		return nil, errors.New("学校设备授权链接不在可信认证站点内")
	}
	interval := max(5, response.Interval)
	now := service.now()
	return &DeviceLogin{VerificationURL: verificationURL, UserCode: response.UserCode, ExpiresAt: now.Add(time.Duration(response.Expires) * time.Second), Interval: interval, document: document, code: response.Code, started: now, nextPoll: now.Add(time.Duration(interval) * time.Second)}, nil
}

// PollDeviceLogin enforces RFC 8628 intervals server-side. Pending/slow_down
// responses are not failures; every terminal error consumes the device code.
func (service *Service) PollDeviceLogin(ctx context.Context, login *DeviceLogin) (Account, bool, error) {
	login.mu.Lock()
	defer login.mu.Unlock()
	now := service.now()
	if login.code == "" || !now.Before(login.ExpiresAt) {
		login.code = ""
		return Account{}, false, errors.New("设备授权已结束或过期，请重新开始")
	}
	if now.Before(login.nextPoll) {
		return Account{}, false, nil
	}
	login.nextPoll = now.Add(time.Duration(login.Interval) * time.Second)
	status, body, err := service.deviceRequest(ctx, login.document.Token, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {"tokenworks"}, "device_code": {login.code}})
	if err != nil {
		if status != 0 {
			login.code = ""
			return Account{}, false, errors.New("学校设备授权响应无效，请重新开始")
		}
		login.Interval = min(300, login.Interval*2)
		login.nextPoll = service.now().Add(time.Duration(login.Interval) * time.Second)
		return Account{}, false, nil
	}
	if status != http.StatusOK {
		var response struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &response)
		if status == http.StatusBadRequest && response.Error == "authorization_pending" {
			return Account{}, false, nil
		}
		if status == http.StatusBadRequest && response.Error == "slow_down" {
			login.Interval = min(300, login.Interval+5)
			login.nextPoll = service.now().Add(time.Duration(login.Interval) * time.Second)
			return Account{}, false, nil
		}
		if status == http.StatusTooManyRequests || status >= 500 {
			login.Interval = min(300, login.Interval*2)
			login.nextPoll = service.now().Add(time.Duration(login.Interval) * time.Second)
			return Account{}, false, nil
		}
		login.code = ""
		return Account{}, false, errors.New("学校拒绝了设备授权或授权已过期，请重新开始")
	}
	login.code = ""
	var token tokenResponse
	if json.Unmarshal(body, &token) != nil || token.Access == "" || token.Refresh == "" || token.ID == "" || !strings.EqualFold(token.Type, "bearer") || token.Expires <= 0 || token.Expires > 365*86400 || strings.ContainsAny(token.Access+token.Refresh, "\r\n") {
		return Account{}, false, errors.New("学校设备授权缺少有效的登录或自动刷新令牌")
	}
	account, err := service.identityAccount(ctx, login.document, token, "", login.started)
	return account, err == nil, err
}

func (service *Service) deviceRequest(parent context.Context, endpoint string, values url.Values) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, nil, errors.New("设备授权请求格式无效")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cpa-ustc/"+Version)
	response, err := service.identity.Do(request)
	if err != nil {
		return 0, nil, errors.New("学校设备授权连接失败")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return response.StatusCode, nil, errors.New("学校设备授权响应过大或读取失败")
	}
	return response.StatusCode, body, nil
}
