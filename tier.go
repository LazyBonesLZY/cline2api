package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 账号订阅层级（tier）探测
//
// 判据来自 GET /users/me/plan（Cline 官方接口，一次廉价的 HTTP 调用）：
//   - 404 "no plan history found for user"        → free（无订阅）
//   - 200 且 plan.entitlements.cline_pass.enabled → pass（可服务付费模型）
//
// 为什么不用「请求付费模型看是否 403」：那会真实消耗上游配额、每个号都要打一次
// 模型请求，实测大量账号被 429 限流（返回 HTML 429 页）而测不出层级。
// plan 接口不消耗 token、不触发模型侧限流，结果还更丰富（计划名/是否启用/到期时间）。
// ============================================================================

const (
	tierUnknown = ""
	tierFree    = "free"
	tierPass    = "pass"
)

// tierCheckTTL 已探测过的账号多久后允许重新探测。
// 订阅状态变化不频繁（月付），默认 24 小时；后台可手动强制重测。
const tierCheckTTL = 24 * time.Hour

// tierDetectWorkers 探测并发度。plan 接口很轻，适度并发可让 600 个号在 1 分钟内跑完。
const tierDetectWorkers = 4

// clinePlanResponse GET /users/me/plan 的响应。
type clinePlanResponse struct {
	Data struct {
		Plan *struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			IsActive    bool   `json:"isActive"`
			// entitlements 形如 {"cline_pass": {"enabled": true}}
			Entitlements map[string]struct {
				Enabled bool `json:"enabled"`
			} `json:"entitlements"`
		} `json:"plan"`
		SubscriptionID   string `json:"subscriptionId"`
		CurrentPeriodEnd string `json:"currentPeriodEnd"`
		CanceledAt       string `json:"canceledAt"`
	} `json:"data"`
	Error   string `json:"error"`
	Success bool   `json:"success"`
}

var (
	tierDetectMu      sync.Mutex
	tierDetectRunning bool
	tierDetectState   = tierDetectProgress{}
)

type tierDetectProgress struct {
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Pass      int       `json:"pass"`
	Free      int       `json:"free"`
	Unknown   int       `json:"unknown"`
	StartedAt time.Time `json:"startedAt"`
	Finished  bool      `json:"finished"`
	LastError string    `json:"lastError,omitempty"`
}

// normalizeTier 归一化层级值。
func normalizeTier(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case tierFree:
		return tierFree
	case tierPass:
		return tierPass
	default:
		return tierUnknown
	}
}

// accountTier 读取账号层级（空 = unknown）。
func accountTier(a *Account) string {
	if a == nil {
		return tierUnknown
	}
	return normalizeTier(a.Tier)
}

// setAccountTier 写入账号层级（持锁）。
func setAccountTier(acc *Account, tier string) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.Tier = normalizeTier(tier)
	acc.TierCheckedAt = time.Now()
	poolMu.Unlock()
}

// applyPlanToAccount 把 plan 查询结果写入账号（层级 + 计划信息）。
func applyPlanToAccount(acc *Account, tier string, plan *clinePlanResponse) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.Tier = normalizeTier(tier)
	acc.TierCheckedAt = time.Now()
	if plan != nil && plan.Data.Plan != nil {
		name := plan.Data.Plan.DisplayName
		if name == "" {
			name = plan.Data.Plan.Name
		}
		acc.PlanName = name
		acc.PlanActive = plan.Data.Plan.IsActive
		acc.PlanPeriodEnd = parsePlanTime(plan.Data.CurrentPeriodEnd)
	} else {
		acc.PlanName = ""
		acc.PlanActive = false
		acc.PlanPeriodEnd = time.Time{}
	}
	poolMu.Unlock()
}

// parsePlanTime 解析 RFC3339（含小数秒）时间，失败返回零值。
func parsePlanTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// fetchAccountPlan 查询账号订阅计划。
// 返回 (tier, plan, err)：err 非空表示查询失败（层级未定，保持 unknown）。
func fetchAccountPlan(acc *Account) (string, *clinePlanResponse, error) {
	token, err := ensureAccountToken(acc)
	if err != nil {
		return tierUnknown, nil, fmt.Errorf("token: %w", err)
	}

	req, err := http.NewRequest("GET", clineAPIBase+"/users/me/plan", nil)
	if err != nil {
		return tierUnknown, nil, err
	}
	req.Header = clineHeaders(token, "")

	resp, err := doUpstream(req, false)
	if err != nil {
		return tierUnknown, nil, fmt.Errorf("plan request: %w", err)
	}
	defer resp.Body.Close()

	// 无计划：Cline 用 404 + 明确文案表达「这个号没有订阅」
	if resp.StatusCode == http.StatusNotFound {
		var probe struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&probe)
		if strings.Contains(strings.ToLower(probe.Error), "no plan") {
			return tierFree, nil, nil
		}
		return tierUnknown, nil, fmt.Errorf("plan 404: %s", truncate(probe.Error, 80))
	}
	if resp.StatusCode != http.StatusOK {
		return tierUnknown, nil, &planHTTPError{status: resp.StatusCode}
	}

	var out clinePlanResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return tierUnknown, nil, fmt.Errorf("plan decode: %w", err)
	}
	if out.Data.Plan == nil {
		// 200 但没有 plan 对象：视为无订阅
		return tierFree, &out, nil
	}
	// cline_pass 权益启用才算 pass；有计划但未启用（如已取消到期）按 free 处理
	if e, ok := out.Data.Plan.Entitlements["cline_pass"]; ok && e.Enabled && out.Data.Plan.IsActive {
		return tierPass, &out, nil
	}
	return tierFree, &out, nil
}

// planHTTPError plan 接口的非 200 响应（429 可重试）。
type planHTTPError struct{ status int }

func (e *planHTTPError) Error() string { return fmt.Sprintf("plan status %d", e.status) }

// planRetryWaits plan 接口遇 429 的退避等待序列。
// 实测 600 个号并发探测会触发上游限流，退避重试能把大多数号测出来。
var planRetryWaits = []time.Duration{2 * time.Second, 6 * time.Second, 15 * time.Second}

// detectAccountTier 探测并写入单个账号的层级。
// 遇 429 限流按 planRetryWaits 退避重试：限流不是层级信息，不能据此判 free/unknown。
func detectAccountTier(acc *Account) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= len(planRetryWaits); attempt++ {
		tier, plan, err := fetchAccountPlan(acc)
		if err == nil {
			applyPlanToAccount(acc, tier, plan)
			return tier, nil
		}
		lastErr = err
		var he *planHTTPError
		if errors.As(err, &he) && he.status == http.StatusTooManyRequests && attempt < len(planRetryWaits) {
			time.Sleep(planRetryWaits[attempt])
			continue
		}
		break
	}
	return tierUnknown, lastErr
}

// startTierDetect 异步批量探测：只探测「未探测过」或「已过 TTL」的账号。
// force 为真时忽略 TTL，全部重测。
func startTierDetect(force bool) bool {
	tierDetectMu.Lock()
	if tierDetectRunning {
		tierDetectMu.Unlock()
		return false
	}
	tierDetectRunning = true
	tierDetectMu.Unlock()

	go func() {
		defer func() {
			tierDetectMu.Lock()
			tierDetectRunning = false
			tierDetectState.Finished = true
			tierDetectMu.Unlock()
			savePool()
		}()

		p := loadPool()
		poolMu.Lock()
		targets := make([]*Account, 0, len(p.Accounts))
		now := time.Now()
		for _, a := range p.Accounts {
			if !force && accountTier(a) != tierUnknown && now.Sub(a.TierCheckedAt) < tierCheckTTL {
				continue
			}
			targets = append(targets, a)
		}
		poolMu.Unlock()

		tierDetectMu.Lock()
		tierDetectState = tierDetectProgress{Total: len(targets), StartedAt: time.Now()}
		tierDetectMu.Unlock()

		log.Printf("tier detect: probing %d account(s) via /users/me/plan, workers=%d",
			len(targets), tierDetectWorkers)

		var (
			wg      sync.WaitGroup
			sem     = make(chan struct{}, tierDetectWorkers)
			doneMu  sync.Mutex
			doneCnt int
		)
		for _, acc := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(a *Account) {
				defer wg.Done()
				defer func() { <-sem }()

				tier, err := detectAccountTier(a)
				tierDetectMu.Lock()
				tierDetectState.Done++
				switch tier {
				case tierPass:
					tierDetectState.Pass++
				case tierFree:
					tierDetectState.Free++
				default:
					tierDetectState.Unknown++
					if err != nil {
						tierDetectState.LastError = err.Error()
					}
				}
				tierDetectMu.Unlock()

				doneMu.Lock()
				doneCnt++
				if doneCnt%25 == 0 {
					savePool()
				}
				doneMu.Unlock()
			}(acc)
		}
		wg.Wait()

		tierDetectMu.Lock()
		pass, free, unk := tierDetectState.Pass, tierDetectState.Free, tierDetectState.Unknown
		lastErr := tierDetectState.LastError
		tierDetectMu.Unlock()
		log.Printf("tier detect done: pass=%d free=%d unknown=%d lastErr=%q", pass, free, unk, truncate(lastErr, 120))
	}()
	return true
}

// tierDetectStatus 返回探测进度（供后台轮询）。
func tierDetectStatus() map[string]any {
	tierDetectMu.Lock()
	defer tierDetectMu.Unlock()
	return map[string]any{
		"running":   tierDetectRunning,
		"total":     tierDetectState.Total,
		"done":      tierDetectState.Done,
		"pass":      tierDetectState.Pass,
		"free":      tierDetectState.Free,
		"unknown":   tierDetectState.Unknown,
		"startedAt": tierDetectState.StartedAt,
		"finished":  tierDetectState.Finished,
		"lastError": tierDetectState.LastError,
	}
}

// tierCounts 统计池中各层级的账号数（供后台展示与筛选）。
func tierCounts() map[string]int {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	out := map[string]int{tierFree: 0, tierPass: 0, "unknown": 0}
	for _, a := range p.Accounts {
		switch accountTier(a) {
		case tierFree:
			out[tierFree]++
		case tierPass:
			out[tierPass]++
		default:
			out["unknown"]++
		}
	}
	return out
}
