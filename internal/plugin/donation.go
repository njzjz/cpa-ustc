package plugin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-ustc/internal/ustc"
)

const donationLifetime = 10 * time.Minute

// donationPortal runs only behind CPA management authentication. A separate
// restricted gateway authenticates server-to-server on behalf of donors, while
// these checks still bind each action to browser origin, session and consent.
type donationPortal struct {
	mu       sync.Mutex
	poolMu   sync.Mutex
	sessions map[string]*donationSession
	starts   []time.Time
	origin   string
	secure   bool
	now      func() time.Time
	start    func(context.Context) (*ustc.DeviceLogin, error)
	poll     func(context.Context, *ustc.DeviceLogin) (ustc.Account, bool, error)
	verify   func(context.Context, ustc.Account) (ustc.Account, error)
	forget   func(string)
}

type donationSession struct {
	mu      sync.Mutex
	csrf    string
	expires time.Time
	state   string
	action  string
	device  *ustc.DeviceLogin
	account ustc.Account
	message string
	confirm string
}

func newDonationPortal(service *ustc.Service) *donationPortal {
	return &donationPortal{sessions: map[string]*donationSession{}, origin: strings.TrimSuffix(service.Config.DonationOrigin, "/"), secure: strings.HasPrefix(service.Config.DonationOrigin, "https://"), now: time.Now, start: service.StartDeviceLogin, poll: service.PollDeviceLogin, forget: service.ForgetAccount, verify: func(ctx context.Context, account ustc.Account) (ustc.Account, error) {
		_, updated, err := service.Resolve(ctx, account)
		return updated, err
	}}
}

func (portal *donationPortal) close() {
	portal.mu.Lock()
	sessions := portal.sessions
	portal.sessions = map[string]*donationSession{}
	portal.mu.Unlock()
	for _, session := range sessions {
		session.mu.Lock()
		session.clear()
		session.mu.Unlock()
	}
}

func (session *donationSession) clear() {
	if session.device != nil {
		session.device.Close()
		session.device = nil
	}
	session.account = ustc.Account{}
	session.confirm = ""
}

func donationRandom() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (portal *donationPortal) cookieName() string {
	if portal.secure {
		return "__Secure-ustc_donation"
	}
	return "ustc_donation_dev"
}

func (portal *donationPortal) session(request rpcRequest, create bool) (*donationSession, string, error) {
	portal.mu.Lock()
	defer portal.mu.Unlock()
	now := portal.now()
	for identifier, session := range portal.sessions {
		if !now.Before(session.expires) && session.mu.TryLock() {
			session.clear()
			session.mu.Unlock()
			delete(portal.sessions, identifier)
		}
	}
	identifier := ""
	for _, cookie := range (&http.Request{Header: request.Headers}).Cookies() {
		if cookie.Name == portal.cookieName() {
			if identifier != "" {
				return nil, "", errors.New("捐献会话 Cookie 无效，请清除该站点 Cookie 后重试")
			}
			identifier = cookie.Value
		}
	}
	if session := portal.sessions[identifier]; session != nil {
		return session, "", nil
	}
	if !create {
		return nil, "", errors.New("捐献会话已过期，请刷新页面重新开始")
	}
	if len(portal.sessions) >= 64 {
		return nil, "", errors.New("捐献会话已满，请稍后重试")
	}
	identifier, err := donationRandom()
	if err != nil {
		return nil, "", errors.New("无法创建安全会话")
	}
	csrf, err := donationRandom()
	if err != nil {
		return nil, "", errors.New("无法创建安全会话")
	}
	session := &donationSession{csrf: csrf, expires: now.Add(donationLifetime), state: "idle"}
	portal.sessions[identifier] = session
	cookie := &http.Cookie{Name: portal.cookieName(), Value: identifier, Path: donationPublicPrefix, HttpOnly: true, Secure: portal.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(donationLifetime.Seconds())}
	return session, cookie.String(), nil
}

func (portal *donationPortal) allowStart() bool {
	portal.mu.Lock()
	defer portal.mu.Unlock()
	now := portal.now()
	recent := portal.starts[:0]
	for _, started := range portal.starts {
		if now.Sub(started) < time.Minute {
			recent = append(recent, started)
		}
	}
	portal.starts = recent
	if len(recent) >= 6 {
		return false
	}
	portal.starts = append(portal.starts, now)
	return true
}

func donationJSON(status int, data any) managementResponse {
	response := webJSON(status, data)
	response.Headers.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	response.Headers.Set("Pragma", "no-cache")
	response.Headers.Set("Vary", "Origin, Sec-Fetch-Site")
	return response
}

func donationError(status int, message string) managementResponse {
	return donationJSON(status, map[string]string{"error": message})
}

func (portal *donationPortal) handle(ctx context.Context, host HostCall, request rpcRequest) managementResponse {
	operation := strings.TrimPrefix(request.Path, donationManagementPrefix)
	if operation != "session" && operation != "start" && operation != "poll" && operation != "confirm" {
		return donationError(404, "接口不存在")
	}
	// Fetch Metadata is browser-controlled. Requiring it also rejects cross-site
	// fetches even when the CPA host has permissive global CORS headers.
	if request.Headers.Get("Sec-Fetch-Site") != "same-origin" || request.Headers.Get("X-USTC-Donation-Origin") != portal.origin || (request.Headers.Get("Origin") != "" && request.Headers.Get("Origin") != portal.origin) || (request.Headers.Get("Sec-Fetch-Mode") != "same-origin" && request.Headers.Get("Sec-Fetch-Mode") != "cors") {
		return donationError(403, "仅允许从本站捐献页面发起请求，请检查管理员配置的 donation-origin")
	}
	session, cookie, err := portal.session(request, operation == "session")
	if err != nil {
		return donationError(429, err.Error())
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !portal.now().Before(session.expires) {
		session.clear()
		return donationError(410, "捐献会话已过期，请刷新页面")
	}
	if operation == "session" {
		response := donationJSON(200, map[string]any{"csrf": session.csrf, "status": session.state})
		if cookie != "" {
			response.Headers.Set("Set-Cookie", cookie)
		}
		return response
	}
	if subtle.ConstantTimeCompare([]byte(request.Headers.Get("X-USTC-Donation-CSRF")), []byte(session.csrf)) != 1 {
		return donationError(403, "捐献会话校验失败，请刷新页面")
	}
	switch operation {
	case "start":
		action := request.Headers.Get("X-USTC-Donation-Action")
		if (action != "donate" && action != "withdraw") || (action == "donate" && request.Headers.Get("X-USTC-Donation-Consent") != "share") {
			return donationError(400, "请明确同意模型额度共享，或选择撤回自己的捐献")
		}
		if session.state != "idle" && session.state != "error" && session.state != "done" {
			return donationError(409, "本次会话已开始，请完成授权或刷新页面等待会话过期")
		}
		if !portal.allowStart() {
			return donationError(429, "授权请求过于频繁，请一分钟后重试")
		}
		device, err := portal.start(ctx)
		if err != nil {
			return donationError(502, "学校设备授权暂不可用，请稍后重试")
		}
		session.device, session.state, session.action = device, "pending", action
		session.message = ""
		return session.snapshot()
	case "poll":
		if session.state == "pending" {
			account, ready, err := portal.poll(ctx, session.device)
			if err != nil {
				session.clear()
				session.state, session.message = "error", "学校授权被拒绝、校验失败或已过期，请重新开始"
			} else if ready {
				if account.Validate() != nil || account.APIKey != "" || account.APIKeyEnv != "" || account.DonationRevoked || !strings.HasPrefix(account.ID, "ustc-sso-") {
					session.clear()
					session.state, session.message = "error", "学校返回的身份无效"
				} else {
					confirmation, err := donationRandom()
					if err != nil {
						session.clear()
						session.state = "error"
						return donationError(503, "无法创建安全确认，请重新开始")
					}
					account.ID = strings.Replace(account.ID, "ustc-sso-", "ustc-donation-", 1)
					session.account, session.state = account, "ready"
					session.confirm = confirmation
					session.device.Close()
					session.device = nil
				}
			}
		}
		return session.snapshot()
	case "confirm":
		if session.state != "ready" || session.confirm == "" || subtle.ConstantTimeCompare([]byte(request.Headers.Get("X-USTC-Donation-Confirm")), []byte(session.confirm)) != 1 {
			return donationError(409, "请先完成本次学校认证；每次确认只能执行一次")
		}
		// Consume before any host write so retries or partial host failures can
		// never replay a credential mutation with an old browser confirmation.
		account, action := session.account, session.action
		session.clear()
		session.state = "done"
		message, err := portal.commit(ctx, host, request.HostCallbackID, account, action)
		if err != nil {
			session.state, session.message = "error", err.Error()
			return donationError(409, err.Error())
		}
		session.message = message
		return session.snapshot()
	}
	return donationError(404, "接口不存在")
}

func (session *donationSession) snapshot() managementResponse {
	result := map[string]any{"status": session.state, "action": session.action}
	if session.state == "pending" && session.device != nil {
		result["verification_url"], result["user_code"] = session.device.VerificationURL, session.device.UserCode
		result["interval"], result["expires_at"] = session.device.Interval, session.device.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if session.state == "ready" {
		result["label"] = session.account.Label
		result["confirmation"] = session.confirm
	}
	if session.message != "" {
		result["message"] = session.message
	}
	return donationJSON(200, result)
}

// commit derives every filename from a freshly verified identity. It never
// accepts a browser-supplied account index, filename, token or credential JSON.
func (portal *donationPortal) commit(ctx context.Context, host HostCall, callbackID string, account ustc.Account, action string) (string, error) {
	portal.poolMu.Lock()
	defer portal.poolMu.Unlock()
	if host == nil {
		return "", errors.New("CPA 号池暂不可用")
	}
	var listing struct {
		Files []struct {
			Name      string `json:"name"`
			AuthIndex string `json:"auth_index"`
		} `json:"files"`
	}
	if host("host.auth.list", map[string]any{"host_callback_id": callbackID}, &listing) != nil {
		return "", errors.New("无法读取 CPA 号池，请联系管理员")
	}
	name := account.ID + ".json"
	privateName := strings.Replace(name, "ustc-donation-", "ustc-sso-", 1)
	var existing *ustc.Account
	count := 0
	for _, entry := range listing.Files {
		if strings.HasPrefix(entry.Name, "ustc-donation-") {
			count++
		}
		if action == "donate" && entry.Name == privateName {
			return "", errors.New("此身份已由管理员添加，捐献入口不会覆盖已有凭据")
		}
		if entry.Name != name {
			continue
		}
		var stored struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if entry.AuthIndex == "" || host("host.auth.get", map[string]any{"host_callback_id": callbackID, "auth_index": entry.AuthIndex}, &stored) != nil {
			return "", errors.New("无法核对已有捐献，请联系管理员")
		}
		var previous ustc.Account
		if stored.Name != name || json.Unmarshal(stored.JSON, &previous) != nil || previous.Validate() != nil || !previous.Donated || previous.ID != account.ID || previous.Subject != account.Subject || previous.Issuer != account.Issuer {
			return "", errors.New("已有记录不属于本次捐献，不允许修改")
		}
		existing = &previous
	}
	if action == "withdraw" {
		if existing == nil || existing.DonationRevoked {
			return "该身份没有有效捐献，无需撤回", nil
		}
		account = *existing
		account.AccessToken, account.RefreshToken, account.APIKey, account.APIKeyEnv = "", "", "", ""
		account.ExpiresAt = time.Time{}
		account.DonationRevoked = true
		if account.CPAFields == nil {
			account.CPAFields = map[string]json.RawMessage{}
		}
		account.CPAFields["disabled"] = json.RawMessage("true")
		withdrawn, _ := json.Marshal(portal.now().UTC().Format(time.RFC3339))
		account.CPAFields["donation_withdrawn_at"] = withdrawn
	} else {
		if existing != nil {
			if existing.DonationRevoked {
				return "", errors.New("此捐献已撤回；请联系管理员删除停用记录后再授权，入口不会自行重新启用账号")
			}
			return "此身份已在捐献号池中，无需重复添加；现有设置保持不变", nil
		}
		if count >= 256 {
			return "", errors.New("捐献号池已满，请联系管理员")
		}
		account.Donated = true
		consented, _ := json.Marshal(portal.now().UTC().Format(time.RFC3339))
		account.CPAFields = map[string]json.RawMessage{"donation_consent_at": consented, "donation_consent_version": json.RawMessage("1")}
		updated, err := portal.verify(ctx, account)
		if err != nil {
			return "", errors.New("学校尚未授予此身份模型访问权限，未加入号池")
		}
		account = updated
	}
	if account.Validate() != nil {
		return "", errors.New("捐献凭据校验失败")
	}
	data, err := json.Marshal(account)
	if err != nil || host("host.auth.save", map[string]any{"host_callback_id": callbackID, "name": name, "json": json.RawMessage(data)}, nil) != nil {
		return "", errors.New("无法保存捐献状态，请联系管理员核对；不要重复确认")
	}
	if action == "withdraw" {
		if portal.forget != nil {
			portal.forget(account.ID)
		}
		return "捐献已撤回：本 CPA 凭据已停用并清除令牌；已发出的请求无法追回，学校侧授权可另行撤销", nil
	}
	return "捐献成功，账号已加入 CPA 原生号池；可再次通过学校认证撤回", nil
}
