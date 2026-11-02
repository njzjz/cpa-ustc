package ustc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Quota follows CPA's normalized quota-provider wire contract. Amounts are
// upstream budget units, not token counts or an assumed currency.
type Quota struct {
	Summary []QuotaMetric `json:"summary,omitempty"`
	Groups  []QuotaGroup  `json:"groups,omitempty"`
}

// QuotaMetric supplies a numeric balance to CPA's account summary cards.
type QuotaMetric struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Format string  `json:"format"`
}

// QuotaGroup keeps key limits separate from shared account limits. They must
// not be summed: every applicable window constrains the same model requests.
type QuotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []QuotaBucket `json:"buckets"`
}

// QuotaBucket describes a known finite upstream budget window.
type QuotaBucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description"`
}

// FetchQuota uses the official TokenWorks identity endpoint, never a model
// request or a guessed gateway billing API. Rotated tokens are returned even
// when usage retrieval fails, so callers can persist them before reporting it.
func (service *Service) FetchQuota(parent context.Context, account Account) (Quota, Account, error) {
	if err := account.Validate(); err != nil {
		return Quota{}, account, err
	}
	if account.APIKey != "" || account.APIKeyEnv != "" {
		return Quota{}, account, &StatusError{Status: 501, Message: "仅 API Key 无法查询额度：学校用量接口需要科大统一认证，请添加统一认证账号，或到学校控制台查看"}
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	entry := service.cached(account)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	current, err := service.oauthAccount(ctx, entry, account)
	if err != nil {
		return Quota{}, current, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, service.usageURL, nil)
	if err != nil {
		return Quota{}, current, errors.New("学校额度查询地址无效")
	}
	request.Header.Set("Authorization", "Bearer "+current.AccessToken)
	request.Header.Set("User-Agent", "cpa-ustc/"+Version)
	request.Header.Set("Accept", "application/json")
	response, err := service.identity.Do(request)
	if err != nil {
		return Quota{}, current, errors.New("学校额度查询失败，请检查网络或稍后重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Quota{}, current, responseStatusError(response, "学校额度查询被拒绝；请稍后重试，401/403 时重新登录科大账号")
	}
	const maxBody = 256 << 10
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return Quota{}, current, errors.New("学校额度响应读取失败或超过大小上限")
	}
	quota, err := parseQuota(body)
	return quota, current, err
}

type usageWindow struct {
	Duration     string          `json:"budget_duration"`
	Limit        json.RawMessage `json:"max_budget"`
	Spend        json.RawMessage `json:"current_spend"`
	AccountSpend json.RawMessage `json:"spend"`
	ResetAt      json.RawMessage `json:"reset_at"`
	Status       string          `json:"status"`
	ErrorCode    string          `json:"error_code"`
	Freshness    string          `json:"freshness"`
	LimitStatus  string          `json:"limit_status"`
	Scope        string          `json:"scope"`
	Shared       bool            `json:"shared"`
}

func usageReady(status, code, freshness string) bool {
	return (status == "" || status == "ok") && code == "" && (freshness == "" || freshness == "fresh")
}

var budgetDuration = regexp.MustCompile(`^\d+(?:\.\d+)?\s*(?:mo|[smhdwy])$`)

func parseQuota(body []byte) (Quota, error) {
	var document struct {
		Status    string          `json:"status"`
		Freshness string          `json:"freshness"`
		UpdatedAt json.RawMessage `json:"data_updated_at"`
		Key       *struct {
			Status      string        `json:"status"`
			UsageStatus string        `json:"usage_status"`
			ErrorCode   string        `json:"error_code"`
			Freshness   string        `json:"freshness"`
			Windows     []usageWindow `json:"budget_windows"`
		} `json:"key"`
		Account *usageWindow `json:"account"`
	}
	if json.Unmarshal(body, &document) != nil || document.Key == nil || len(document.Key.Windows) > 64 {
		return Quota{}, errors.New("学校额度响应格式无效")
	}
	unavailable := errors.New("学校额度快照尚未就绪、已过期或同步异常，请稍后重试；未知余额不会显示为零")
	if (document.Status != "ok" && document.Status != "partial") || (document.Freshness != "" && document.Freshness != "fresh") || !usageReady(document.Key.UsageStatus, document.Key.ErrorCode, document.Key.Freshness) {
		return Quota{}, unavailable
	}
	if document.Key.Status != "" && document.Key.Status != "active" {
		return Quota{}, errors.New("学校额度关联的 Key 已停用或不可用，请到学校平台检查账号")
	}
	var result Quota
	appendGroup := func(name, prefix string, windows []usageWindow, account bool) error {
		group := QuotaGroup{DisplayName: name}
		for index, window := range windows {
			if window.LimitStatus == "unlimited" {
				continue
			}
			if !usageReady(window.Status, window.ErrorCode, window.Freshness) {
				return unavailable
			}
			spendRaw := window.Spend
			if account {
				spendRaw = window.AccountSpend
			}
			limit, limitKnown := budgetNumber(window.Limit)
			spend, spendKnown := budgetNumber(spendRaw)
			duration := strings.ToLower(strings.TrimSpace(window.Duration))
			if !limitKnown || limit <= 0 || !spendKnown || spend < 0 || len(duration) > 24 || !budgetDuration.MatchString(duration) {
				return unavailable
			}
			remaining := limit - spend
			description := fmt.Sprintf("已用 %g / %g；剩余 %g（平台计费额度，非 token 数）", spend, limit, remaining)
			if document.Status == "partial" {
				description += "；仅部分额度快照可用，未展示部分状态未知"
			}
			if updated := budgetTime(document.UpdatedAt); updated != "" {
				description += "；快照更新于 " + updated
			}
			group.Buckets = append(group.Buckets, QuotaBucket{
				Window: duration, RemainingFraction: math.Max(0, math.Min(1, 1-spend/limit)),
				ResetTime: budgetTime(window.ResetAt), Description: description,
			})
			result.Summary = append(result.Summary, QuotaMetric{
				Key: fmt.Sprintf("%s_%d_remaining", prefix, index), Label: name + " " + duration + " 剩余",
				Value: remaining, Unit: "额度", Format: "number",
			})
		}
		if len(group.Buckets) > 0 {
			result.Groups = append(result.Groups, group)
		}
		return nil
	}
	if err := appendGroup("API Key 限额", "key", document.Key.Windows, false); err != nil {
		return Quota{}, err
	}
	if document.Account != nil {
		name := "账户额度"
		if document.Account.Shared {
			name = "共享账户额度"
		}
		if document.Account.Scope == "user" || document.Account.Scope == "project" {
			name = "用户"
			if document.Account.Scope == "project" {
				name = "项目"
			}
			if document.Account.Shared {
				name += "共享"
			}
			name += "额度"
		}
		if err := appendGroup(name, "account", []usageWindow{*document.Account}, true); err != nil {
			return Quota{}, err
		}
	}
	if len(result.Groups) == 0 {
		return Quota{}, errors.New("学校未返回可显示的有限额度窗口，可能未限额；请到学校平台确认")
	}
	return result, nil
}

// budgetNumber accepts the official client's numeric-string Decimal variant,
// but never turns null/missing/invalid amounts into a zero or unlimited budget.
func budgetNumber(raw json.RawMessage) (float64, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = string(raw)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// budgetTime accepts zoned ISO timestamps and epoch seconds/milliseconds as in
// TokenWorks. Missing/invalid reset times stay unknown, never guessed locally.
func budgetTime(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err == nil {
			return stamp.UTC().Format(time.RFC3339Nano)
		}
	}
	value, known := budgetNumber(bytes.TrimSpace(raw))
	if !known || value <= 0 {
		return ""
	}
	if value >= 1e12 {
		value /= 1000
	}
	if value >= 253402300800 {
		return ""
	}
	return time.Unix(int64(value), 0).UTC().Format(time.RFC3339)
}
