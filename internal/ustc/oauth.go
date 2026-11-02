package ustc

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const contentBase = "https://id.ustc.edu.cn/doc/claude/tokenworks-content/v2"
const publisherKey = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAQaBXGreNtCH3e1iRrPECbtZofjjB/OJefrBKgEpQ5lI=\n-----END PUBLIC KEY-----\n"

func schoolURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && trustedURL(raw) == nil && (parsed.Hostname() == "ustc.edu.cn" || strings.HasSuffix(parsed.Hostname(), ".ustc.edu.cn")) && (parsed.Port() == "" || parsed.Port() == "443")
}

func randomText(size int) string {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// CheckPublishedProfile authenticates the public login client using USTC's pinned signing key.
func (service *Service) CheckPublishedProfile(ctx context.Context) error {
	var envelope struct {
		Schema    int    `json:"schemaVersion"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := getJSON(ctx, service.identity, contentBase+"/stable/latest.json", "", &envelope); err != nil {
		return err
	}
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return errors.New("学校签名配置格式无效")
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil {
		return errors.New("学校配置签名格式无效")
	}
	block, _ := pem.Decode([]byte(publisherKey))
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}
	if envelope.Schema != 1 || !ed25519.Verify(key.(ed25519.PublicKey), payload, signature) {
		return errors.New("学校配置验签失败；拒绝继续登录")
	}
	var manifest struct {
		Publisher string `json:"publisher"`
		Channel   string `json:"channel"`
		Bundle    struct {
			URL    string `json:"url"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		} `json:"bundle"`
	}
	if json.Unmarshal(payload, &manifest) != nil || manifest.Publisher != "ustc-tokenworks" || manifest.Channel != "stable" || !strings.HasPrefix(manifest.Bundle.URL, contentBase+"/bundles/") || manifest.Bundle.Bytes <= 0 || manifest.Bundle.Bytes > 16<<20 {
		return errors.New("学校配置清单不在信任边界内")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, manifest.Bundle.URL, nil)
	response, err := service.identity.Do(request)
	if err != nil {
		return errors.New("无法下载学校公共登录配置")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(manifest.Bundle.Bytes)+1))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if response.StatusCode != 200 || len(data) != manifest.Bundle.Bytes || hex.EncodeToString(digest[:]) != manifest.Bundle.SHA256 {
		return errors.New("学校配置摘要或大小校验失败")
	}
	var bundle struct {
		Configuration struct {
			Organizations []struct {
				ID   string `json:"id"`
				OIDC struct {
					Issuer   string `json:"issuer"`
					ClientID string `json:"clientId"`
				} `json:"oidc"`
			} `json:"organizations"`
		} `json:"configuration"`
	}
	if json.Unmarshal(data, &bundle) != nil {
		return errors.New("学校公共登录配置格式无效")
	}
	for _, organization := range bundle.Configuration.Organizations {
		if organization.ID == "ustc" && organization.OIDC.Issuer == Issuer && organization.OIDC.ClientID == "tokenworks" {
			return nil
		}
	}
	return errors.New("学校已更改公共登录客户端，请更新插件；拒绝猜测登录配置")
}

type discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	Userinfo      string `json:"userinfo_endpoint"`
	JWKS          string `json:"jwks_uri"`
	RequireIssuer bool   `json:"authorization_response_iss_parameter_supported"`
}

func getJSON(ctx context.Context, client *http.Client, target, token string, destination any) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("学校认证服务连接失败")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return &StatusError{Status: response.StatusCode, Message: "学校认证服务请求失败"}
	}
	if json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(destination) != nil {
		return errors.New("学校认证服务返回无效 JSON")
	}
	return nil
}

func (service *Service) discover(ctx context.Context) (discovery, error) {
	var document discovery
	err := getJSON(ctx, service.identity, Issuer+"/.well-known/openid-configuration", "", &document)
	if err != nil {
		return document, err
	}
	if document.Issuer != Issuer {
		return document, errors.New("OIDC issuer 不匹配")
	}
	for _, endpoint := range []string{document.Authorization, document.Token, document.Userinfo, document.JWKS} {
		if trustedURL(endpoint) != nil || !strings.HasPrefix(endpoint, Issuer+"/") {
			return document, errors.New("OIDC endpoint 不在科大认证信任边界内")
		}
	}
	return document, nil
}

// Login owns a short-lived, loopback-only OAuth callback with single-use PKCE state.
type Login struct {
	URL       string
	State     string
	ExpiresAt time.Time
	mu        sync.Mutex
	account   Account
	err       error
	done      chan struct{}
	once      sync.Once
	started   bool
	server    *http.Server
	redirect  string
}

func (login *Login) finish(account Account, err error) {
	login.once.Do(func() {
		login.mu.Lock()
		login.account = account
		login.err = err
		login.mu.Unlock()
		close(login.done)
	})
}

// Poll is nonblocking and never includes tokens unless authentication has finished.
func (login *Login) Poll() (Account, bool, error) {
	select {
	case <-login.done:
		login.mu.Lock()
		defer login.mu.Unlock()
		return login.account, true, login.err
	default:
		return Account{}, false, nil
	}
}

func (login *Login) Wait(ctx context.Context) (Account, error) {
	select {
	case <-ctx.Done():
		login.finish(Account{}, ctx.Err())
		return Account{}, ctx.Err()
	case <-login.done:
		account, _, err := login.Poll()
		return account, err
	}
}

func (login *Login) Close() {
	login.finish(Account{}, errors.New("登录已取消"))
	_ = login.server.Close()
}

// SubmitCallback supports remote CPA deployments where the browser's loopback
// is not the server's. Only the exact redirect created for this session is used;
// callers cannot turn callback submission into an arbitrary HTTP proxy.
func (login *Login) SubmitCallback(ctx context.Context, raw string) error {
	callback, err := url.Parse(raw)
	expected, _ := url.Parse(login.redirect)
	if err != nil || expected == nil || login.redirect == "" || !time.Now().Before(login.ExpiresAt) || callback.Scheme != expected.Scheme || callback.Host != expected.Host || callback.Path != expected.Path || callback.User != nil || callback.Fragment != "" || len(raw) > 16<<10 {
		return errors.New("请提交本次登录的完整本机回调链接")
	}
	parameters, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(parameters["state"]) != 1 || subtle.ConstantTimeCompare([]byte(parameters.Get("state")), []byte(login.State)) != 1 {
		return errors.New("OAuth state 校验失败")
	}
	expected.RawQuery = callback.RawQuery
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, expected.String(), nil)
	client := HTTPClient("")
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return errors.New("提交 OAuth 回调失败，请重新登录")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("OAuth 回调被拒绝，请检查授权结果或重新登录")
	}
	return nil
}

// StartLogin leaves passwords, passkeys and MFA entirely on the official identity site.
func (service *Service) StartLogin(ctx context.Context, port int) (*Login, error) {
	if port < 0 || port > 65535 {
		return nil, errors.New("callback-port 必须为 0–65535")
	}
	if err := service.CheckPublishedProfile(ctx); err != nil {
		return nil, err
	}
	document, err := service.discover(ctx)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("无法监听本机登录回调: %w", err)
	}
	redirect := "http://" + listener.Addr().String() + "/oauth/callback"
	verifier := randomText(48)
	nonce := randomText(32)
	challenge := sha256.Sum256([]byte(verifier))
	login := &Login{State: randomText(32), ExpiresAt: time.Now().Add(10 * time.Minute), done: make(chan struct{}), redirect: redirect}
	authorization, _ := url.Parse(document.Authorization)
	query := authorization.Query()
	for key, value := range map[string]string{"response_type": "code", "client_id": "tokenworks", "redirect_uri": redirect, "scope": "openid profile email offline_access", "state": login.State, "nonce": nonce, "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256"} {
		query.Set(key, value)
	}
	authorization.RawQuery = query.Encode()
	login.URL = authorization.String()
	login.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	login.server.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'")
		parameters, parseErr := url.ParseQuery(request.URL.RawQuery)
		valid := parseErr == nil && request.Method == http.MethodGet && request.URL.Path == "/oauth/callback" && request.Host == listener.Addr().String() && len(parameters["state"]) == 1 && subtle.ConstantTimeCompare([]byte(parameters.Get("state")), []byte(login.State)) == 1
		if document.RequireIssuer {
			valid = valid && len(parameters["iss"]) == 1 && parameters.Get("iss") == Issuer
		}
		if !valid {
			http.Error(writer, "Invalid OAuth callback", http.StatusBadRequest)
			return
		}
		login.mu.Lock()
		used := login.started
		login.started = true
		login.mu.Unlock()
		if used {
			http.Error(writer, "Login already handled", http.StatusConflict)
			return
		}
		if parameters.Get("error") != "" || len(parameters["code"]) != 1 || parameters.Get("code") == "" {
			login.finish(Account{}, errors.New("学校授权被拒绝或返回的授权码无效"))
			http.Error(writer, "Sign-in failed. Please return to CPA.", 400)
			return
		}
		account, exchangeErr := service.exchange(request.Context(), document, parameters.Get("code"), verifier, nonce, redirect)
		login.finish(account, exchangeErr)
		if exchangeErr != nil {
			http.Error(writer, "Sign-in failed. Please return to CPA.", 400)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(writer, "科大账号登录成功，可以关闭此页面并返回 CLIProxyAPI。")
	})
	go func() {
		if serveErr := login.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			login.finish(Account{}, serveErr)
		}
	}()
	go func() {
		timer := time.NewTimer(time.Until(login.ExpiresAt))
		defer timer.Stop()
		select {
		case <-timer.C:
			login.finish(Account{}, errors.New("登录已超时，请重试"))
		case <-ctx.Done():
			login.finish(Account{}, ctx.Err())
		case <-login.done:
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = login.server.Shutdown(shutdown)
	}()
	return login, nil
}

type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	ID      string `json:"id_token"`
	Type    string `json:"token_type"`
	Expires int64  `json:"expires_in"`
}

func (service *Service) token(ctx context.Context, endpoint string, values url.Values) (tokenResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := service.identity.Do(request)
	if err != nil {
		return tokenResponse{}, errors.New("科大令牌请求失败")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return tokenResponse{}, &StatusError{Status: response.StatusCode, Message: "科大令牌请求被拒绝，请重新登录"}
	}
	var token tokenResponse
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token) != nil || token.Access == "" || !strings.EqualFold(token.Type, "bearer") || token.Expires <= 0 || token.Expires > 365*86400 || strings.ContainsAny(token.Access, "\r\n") {
		return tokenResponse{}, errors.New("学校返回的令牌格式无效")
	}
	return token, nil
}

func (service *Service) exchange(ctx context.Context, document discovery, code, verifier, nonce, redirect string) (Account, error) {
	token, err := service.token(ctx, document.Token, url.Values{"grant_type": {"authorization_code"}, "client_id": {"tokenworks"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
	if err != nil {
		return Account{}, err
	}
	claims, err := service.verifyIDToken(ctx, document, token.ID, nonce, token.Access)
	if err != nil {
		return Account{}, err
	}
	var identity struct {
		Subject string `json:"sub"`
		Name    string `json:"name"`
	}
	if err = getJSON(ctx, service.identity, document.Userinfo, token.Access, &identity); err != nil {
		return Account{}, err
	}
	if identity.Subject != claims.Subject {
		return Account{}, errors.New("OIDC userinfo subject 不匹配")
	}
	digest := sha256.Sum256([]byte(Issuer + "\x00" + identity.Subject))
	label := identity.Name
	if label == "" {
		label = "科大账号"
	}
	return Account{Type: Provider, ID: "ustc-sso-" + hex.EncodeToString(digest[:12]), Label: label, Subject: identity.Subject, Issuer: Issuer, ClientID: "tokenworks", AccessToken: token.Access, RefreshToken: token.Refresh, ExpiresAt: time.Now().Add(time.Duration(token.Expires) * time.Second)}, nil
}

func (service *Service) refresh(ctx context.Context, account Account) (Account, error) {
	if account.RefreshToken == "" {
		return account, errors.New("科大登录已过期，请重新运行 cpa-ustc setup")
	}
	document, err := service.discover(ctx)
	if err != nil {
		return account, err
	}
	token, err := service.token(ctx, document.Token, url.Values{"grant_type": {"refresh_token"}, "client_id": {account.ClientID}, "refresh_token": {account.RefreshToken}})
	if err != nil {
		return account, err
	}
	if token.ID != "" {
		claims, verifyErr := service.verifyIDToken(ctx, document, token.ID, "", token.Access)
		if verifyErr != nil {
			return account, verifyErr
		}
		if claims.Subject != account.Subject {
			return account, errors.New("刷新后的登录身份发生变化")
		}
	}
	account.AccessToken = token.Access
	account.ExpiresAt = time.Now().Add(time.Duration(token.Expires) * time.Second)
	if token.Refresh != "" {
		account.RefreshToken = token.Refresh
	}
	return account, nil
}

type idClaims struct {
	Issuer     string          `json:"iss"`
	Subject    string          `json:"sub"`
	Audience   json.RawMessage `json:"aud"`
	AZP        string          `json:"azp"`
	Nonce      string          `json:"nonce"`
	Expires    int64           `json:"exp"`
	Issued     int64           `json:"iat"`
	NotBefore  int64           `json:"nbf"`
	AccessHash string          `json:"at_hash"`
}

// verifyIDToken validates RS256, issuer/audience, nonce, lifetime and token binding.
func (service *Service) verifyIDToken(ctx context.Context, document discovery, raw, nonce, access string) (idClaims, error) {
	invalid := errors.New("OIDC ID Token 校验失败")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, invalid
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return idClaims{}, invalid
	}
	var header struct {
		Algorithm string   `json:"alg"`
		Key       string   `json:"kid"`
		Critical  []string `json:"crit"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.Key == "" || len(header.Critical) > 0 {
		return idClaims{}, invalid
	}
	var keys struct {
		Keys []struct {
			Type      string `json:"kty"`
			Key       string `json:"kid"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
		} `json:"keys"`
	}
	if err = getJSON(ctx, service.identity, document.JWKS, "", &keys); err != nil {
		return idClaims{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idClaims{}, invalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	verified := false
	for _, key := range keys.Keys {
		if key.Type != "RSA" || key.Key != header.Key || (key.Use != "" && key.Use != "sig") || (key.Algorithm != "" && key.Algorithm != "RS256") {
			continue
		}
		modulus, modErr := base64.RawURLEncoding.DecodeString(key.Modulus)
		exponent, expErr := base64.RawURLEncoding.DecodeString(key.Exponent)
		if modErr != nil || expErr != nil || len(exponent) > 4 {
			continue
		}
		number := new(big.Int).SetBytes(modulus)
		power := new(big.Int).SetBytes(exponent).Int64()
		if number.BitLen() < 2048 || power < 3 || power > 1<<31-1 {
			continue
		}
		if rsa.VerifyPKCS1v15(&rsa.PublicKey{N: number, E: int(power)}, crypto.SHA256, digest[:], signature) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return idClaims{}, invalid
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idClaims{}, invalid
	}
	var claims idClaims
	if json.Unmarshal(data, &claims) != nil {
		return claims, invalid
	}
	var audience []string
	var single string
	if json.Unmarshal(claims.Audience, &single) == nil {
		audience = []string{single}
	} else if json.Unmarshal(claims.Audience, &audience) != nil {
		return claims, invalid
	}
	validAudience := false
	for _, value := range audience {
		if value == "tokenworks" {
			validAudience = true
		}
	}
	now := time.Now().Unix()
	if claims.Issuer != Issuer || claims.Subject == "" || !validAudience || (len(audience) > 1 && claims.AZP != "tokenworks") || (claims.AZP != "" && claims.AZP != "tokenworks") || claims.Expires <= now-30 || claims.Issued <= 0 || claims.Issued > now+30 || claims.NotBefore > now+30 || (nonce != "" && subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1) {
		return claims, invalid
	}
	if claims.AccessHash != "" {
		hash := sha256.Sum256([]byte(access))
		if subtle.ConstantTimeCompare([]byte(claims.AccessHash), []byte(base64.RawURLEncoding.EncodeToString(hash[:16]))) != 1 {
			return claims, invalid
		}
	}
	return claims, nil
}
