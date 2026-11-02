package ustc

import (
	"errors"
	"time"
)

// RecordInferenceError applies budget exhaustion to the entire CPA credential.
// The host currently transports retry hints, but not credential-scoped errors,
// across its plugin ABI. This guard prevents cross-model upstream retries while
// leaving account selection and failover entirely with CPA's native scheduler.
// Callers must also report errors found inside HTTP 200 JSON and SSE responses.
func (service *Service) RecordInferenceError(account Account, err error) error {
	var upstream *StatusError
	if !errors.As(err, &upstream) || !upstream.BudgetExceeded {
		return err
	}
	entry := service.cached(account)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := service.now()
	if !entry.budgetUntil.After(now) {
		delay := time.Duration(upstream.RetryAfter) * time.Second
		if upstream.RetryAfter <= 0 {
			delay, _ = time.ParseDuration(service.Config.BudgetCooldown)
			if delay <= 0 {
				delay, _ = time.ParseDuration(DefaultBudgetCooldown)
			}
		}
		entry.budgetUntil = now.Add(delay)
	}
	// Concurrent in-flight failures reuse the first deadline. Local rejections
	// must not slide the window forward or turn a finite cooldown into a ban.
	return budgetCooldownError(entry.budgetUntil, now)
}

func (service *Service) budgetCooldown(account Account) *StatusError {
	entry := service.cached(account)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := service.now()
	if !entry.budgetUntil.After(now) {
		return nil
	}
	return budgetCooldownError(entry.budgetUntil, now)
}

func budgetCooldownError(deadline, now time.Time) *StatusError {
	delay := deadline.Sub(now)
	seconds := int(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	return &StatusError{Status: 429, Message: "学校模型网关预算额度已用尽（budget_exceeded），凭据冷却中，请等待 CPA 切换账号或冷却后重试", RetryAfter: seconds, BudgetExceeded: true}
}
