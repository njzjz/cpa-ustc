package ustc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Account is CPA's provider-owned auth JSON. Gateway keys from SSO stay in memory.
type Account struct {
	Type         string                     `json:"type"`
	ID           string                     `json:"id"`
	Label        string                     `json:"label"`
	APIKey       string                     `json:"api_key,omitempty"`
	APIKeyEnv    string                     `json:"api_key_env,omitempty"`
	APIBase      string                     `json:"api_base,omitempty"`
	AccessToken  string                     `json:"access_token,omitempty"`
	RefreshToken string                     `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time                  `json:"expires_at,omitempty"`
	Subject      string                     `json:"subject,omitempty"`
	Issuer       string                     `json:"issuer,omitempty"`
	ClientID     string                     `json:"client_id,omitempty"`
	CPAFields    map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON retains CPA-owned routing fields so token rotation does not
// erase disabled flags, priorities, prefixes or other host credential metadata.
func (account *Account) UnmarshalJSON(data []byte) error {
	type plainAccount Account
	var decoded plainAccount
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"type", "id", "label", "api_key", "api_key_env", "api_base", "access_token", "refresh_token", "expires_at", "subject", "issuer", "client_id"} {
		delete(fields, name)
	}
	decoded.CPAFields = fields
	*account = Account(decoded)
	return nil
}

// MarshalJSON merges retained host fields with the provider's current tokens.
func (account Account) MarshalJSON() ([]byte, error) {
	type plainAccount Account
	data, err := json.Marshal(plainAccount(account))
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	for name, value := range account.CPAFields {
		fields[name] = value
	}
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// APIAccount creates either an environment reference or a private key record.
func APIAccount(key, environment string, config Config) (Account, error) {
	if key == "" && environment == "" {
		return Account{}, errors.New("API Key 不能为空")
	}
	if strings.ContainsAny(key, "\r\n") {
		return Account{}, errors.New("API Key 包含非法换行")
	}
	digest := sha256.Sum256([]byte(key + "\x00" + environment + "\x00" + config.APIBase))
	return Account{Type: Provider, ID: "ustc-key-" + hex.EncodeToString(digest[:8]), Label: "USTC API Key", APIKey: key, APIKeyEnv: environment, APIBase: config.APIBase}, nil
}

func (account Account) Validate() error {
	if account.Type != Provider || !accountID.MatchString(account.ID) {
		return errors.New("invalid USTC account")
	}
	if account.APIKey != "" || account.APIKeyEnv != "" {
		return trustedURL(account.APIBase)
	}
	if account.AccessToken == "" || account.Issuer != Issuer || account.ClientID != "tokenworks" || account.Subject == "" || account.ExpiresAt.IsZero() {
		return errors.New("invalid USTC OAuth account; please log in again")
	}
	return nil
}

var accountID = regexp.MustCompile(`^ustc-[A-Za-z0-9_-]{1,120}$`)

// Model describes the bootstrap catalogue and CPA-compatible model metadata.
type Model struct {
	ID      string
	Name    string
	Context int64
	Output  int64
}

// Gateway is short-lived bootstrap configuration, never serialized to auth files.
type Gateway struct {
	Base      string
	Key       string
	Models    []Model
	RefreshAt time.Time
}

type cachedAccount struct {
	mu      sync.Mutex
	account Account
	gateway Gateway
}

// Service serializes token rotation per account and caches bootstrap credentials.
type Service struct {
	Config       Config
	Network      *Network
	mu           sync.Mutex
	accounts     map[string]*cachedAccount
	identity     *http.Client
	bootstrapURL string
	apiClient    func(string) *http.Client
}

func NewService(config Config) *Service {
	return &Service{Config: config, Network: NewNetwork(config), accounts: map[string]*cachedAccount{}, identity: HTTPClient(""), bootstrapURL: BootstrapURL, apiClient: HTTPClient}
}

func (service *Service) Close() { service.Network.Close(); service.identity.CloseIdleConnections() }

func (service *Service) cached(account Account) *cachedAccount {
	service.mu.Lock()
	defer service.mu.Unlock()
	entry := service.accounts[account.ID]
	if entry == nil {
		entry = &cachedAccount{account: account}
		service.accounts[account.ID] = entry
	}
	return entry
}

// Resolve refreshes OAuth before bootstrap and never substitutes an identity token for an API key.
func (service *Service) Resolve(ctx context.Context, account Account) (Gateway, Account, error) {
	if err := account.Validate(); err != nil {
		return Gateway{}, account, err
	}
	entry := service.cached(account)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if account.APIKey != "" || account.APIKeyEnv != "" {
		key := account.APIKey
		if account.APIKeyEnv != "" {
			key = os.Getenv(account.APIKeyEnv)
		}
		if key == "" || strings.ContainsAny(key, "\r\n") {
			return Gateway{}, account, errors.New("API Key 环境变量未设置或格式无效")
		}
		return Gateway{Base: strings.TrimRight(account.APIBase, "/"), Key: key}, account, nil
	}
	current := entry.account
	// A newer login must supersede an older cached refresh token for the same subject.
	if account.ExpiresAt.After(current.ExpiresAt) && account.AccessToken != current.AccessToken {
		entry.account = account
		entry.gateway = Gateway{}
		current = account
	}
	current.CPAFields = account.CPAFields
	entry.account.CPAFields = account.CPAFields
	if time.Until(current.ExpiresAt) < time.Minute {
		updated, err := service.refresh(ctx, current)
		if err != nil {
			return Gateway{}, current, err
		}
		entry.account = updated
		current = updated
	}
	if time.Now().Before(entry.gateway.RefreshAt) {
		return entry.gateway, current, nil
	}
	gateway, err := service.bootstrap(ctx, current)
	if err != nil {
		entry.gateway = Gateway{}
		return Gateway{}, current, err
	}
	entry.gateway = gateway
	return gateway, current, nil
}

func (service *Service) bootstrap(ctx context.Context, account Account) (Gateway, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, service.bootstrapURL, nil)
	request.Header.Set("Authorization", "Bearer "+account.AccessToken)
	request.Header.Set("User-Agent", "cpa-ustc/"+Version)
	response, err := service.identity.Do(request)
	if err != nil {
		return Gateway{}, errors.New("获取学校模型配置失败，请检查网络")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return Gateway{}, &StatusError{Status: response.StatusCode, Message: "学校模型配置被拒绝，请检查账号权限或重新登录"}
	}
	var document struct {
		Provider  string            `json:"inferenceProvider"`
		Kind      string            `json:"inferenceCredentialKind"`
		Base      string            `json:"inferenceGatewayBaseUrl"`
		Key       string            `json:"inferenceGatewayApiKey"`
		Scheme    string            `json:"inferenceGatewayAuthScheme"`
		Headers   map[string]string `json:"inferenceCustomHeaders"`
		Models    []json.RawMessage `json:"inferenceModels"`
		ExpiresAt json.Number       `json:"expiresAt"`
		Recheck   int               `json:"configRecheckIntervalMinutes"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&document); err != nil {
		return Gateway{}, errors.New("学校模型配置格式无效")
	}
	if document.Provider != "gateway" || (document.Kind != "" && document.Kind != "static") || (document.Scheme != "" && document.Scheme != "bearer") || len(document.Headers) > 0 || document.Key == "" || strings.ContainsAny(document.Key, "\r\n") || len(document.Models) == 0 {
		return Gateway{}, errors.New("不支持的学校模型配置；要求 static bearer 网关及模型列表")
	}
	if err = trustedURL(document.Base); err != nil {
		return Gateway{}, errors.New("学校模型网关必须使用 HTTPS")
	}
	parsedBase := apiBase(document.Base)
	// Bootstrap is trusted for school configuration, not arbitrary credential exfiltration.
	if !schoolURL(parsedBase) {
		return Gateway{}, errors.New("学校模型网关不在 ustc.edu.cn 信任边界内")
	}
	interval := document.Recheck
	if interval <= 0 || interval > 60 {
		interval = 10
	}
	gateway := Gateway{Base: parsedBase, Key: document.Key, RefreshAt: time.Now().Add(time.Duration(interval) * time.Minute)}
	if document.ExpiresAt != "" {
		stamp, parseErr := document.ExpiresAt.Float64()
		if stamp >= 1e12 {
			stamp /= 1000
		}
		expiry := time.Unix(int64(stamp), 0)
		if parseErr != nil || stamp <= 0 || !expiry.After(time.Now()) {
			return Gateway{}, errors.New("学校模型配置已过期或有效期无效")
		}
		if expiry.Before(gateway.RefreshAt) {
			gateway.RefreshAt = expiry
		}
	}
	for _, raw := range document.Models {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			gateway.Models = append(gateway.Models, Model{ID: name, Name: name})
			continue
		}
		var model struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Context     int64  `json:"contextWindow"`
			Output      int64  `json:"maxOutputTokens"`
		}
		if json.Unmarshal(raw, &model) != nil || model.Name == "" {
			return Gateway{}, errors.New("学校模型目录格式无效")
		}
		if model.DisplayName == "" {
			model.DisplayName = model.Name
		}
		gateway.Models = append(gateway.Models, Model{ID: model.Name, Name: model.DisplayName, Context: model.Context, Output: model.Output})
	}
	return gateway, nil
}

// Models returns the account's live catalogue rather than guessing model names.
func (service *Service) Models(ctx context.Context, account Account) ([]Model, Account, error) {
	gateway, updated, err := service.Resolve(ctx, account)
	if err != nil {
		return nil, updated, err
	}
	if len(gateway.Models) > 0 {
		return gateway.Models, updated, nil
	}
	proxy, err := service.Network.Proxy(ctx)
	if err != nil {
		return nil, updated, err
	}
	client := service.apiClient(proxy)
	defer client.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, gateway.Base+"/models", nil)
	request.Header.Set("Authorization", "Bearer "+gateway.Key)
	response, err := client.Do(request)
	if err != nil {
		return nil, updated, errors.New("获取模型列表失败，请检查校园网/iWAN")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, updated, &StatusError{Status: response.StatusCode, Message: "获取模型列表失败"}
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&list); err != nil {
		return nil, updated, errors.New("模型列表格式无效")
	}
	models := make([]Model, 0, len(list.Data))
	for _, model := range list.Data {
		if model.ID != "" {
			models = append(models, Model{ID: model.ID, Name: model.ID})
		}
	}
	return models, updated, nil
}

// StatusError preserves upstream status without leaking credential-bearing response bodies.
type StatusError struct {
	Status     int
	Message    string
	RetryAfter int
}

func (err *StatusError) Error() string { return fmt.Sprintf("%s (HTTP %d)", err.Message, err.Status) }
