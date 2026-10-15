package main

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 账号订阅层级（tier）自动探测
//
// 背景：池里绝大多数是免费账号，它们请求 cline-pass/* 付费模型时上游一律返回
// 403 ENTITLEMENT_ERROR。若不做区分，一次付费请求会依次撞过全部账号、
// 每个账号吃一次 403 并被写上模型冷却 —— 既慢又把整池毒化。
//
// 因此：
//  1. 探测并缓存每个账号的层级（free / pass）；
//  2. 付费模型只路由到 pass 账号（未知层级作为待发现候选，命中 403 后自我降级）；
//  3. 任何一次 403 ENTITLEMENT 都会把该账号标记为 free，池子会自我学习。
// ============================================================================

const (
	tierUnknown = ""
	tierFree    = "free"
	tierPass    = "pass"
)

// tierProbeModelFallback 探测用的付费模型（目录里找不到 cline-pass/* 时的兜底）。
const tierProbeModelFallback = "cline-pass/deepseek-v4.1-flash"

// tierCheckTTL 已探测过的账号多久后允许重新探测。
// 订阅状态不会频繁变化，默认 7 天；管理后台可手动强制重测。
const tierCheckTTL = 7 * 24 * time.Hour

var (
	tierDetectMu      sync.Mutex
	tierDetectRunning bool
	tierDetectState   = struct {
		Total     int       `json:"total"`
		Done      int       `json:"done"`
		Pass      int       `json:"pass"`
		Free      int       `json:"free"`
		Unknown   int       `json:"unknown"`
		StartedAt time.Time `json:"startedAt"`
		Finished  bool      `json:"finished"`
	}{}
)

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

// tierProbeModel 选一个付费模型用于探测：优先目录里真实存在的 cline-pass/*。
func tierProbeModel() string {
	for _, m := range getAllModels() {
		if strings.HasPrefix(m.ID, "cline-pass/") && m.Cost != "free" {
			return m.ID
		}
	}
	for _, m := range getAllModels() {
		if m.Cost != "free" && !strings.HasPrefix(m.ID, "cline-") {
			return m.ID
		}
	}
	return tierProbeModelFallback
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

// tierProbeInterval 两次探测之间的间隔。
// 实测并发/连打过密会被上游以 429 限流（返回 HTML 429 页），大量账号因此测不出层级；
// 拉开间隔虽然慢一些，但能拿到准确结果。
const tierProbeInterval = 400 * time.Millisecond

// tierProbeRetryWait 探测遇到 429 时的退避等待（重试一次）。
const tierProbeRetryWait = 3 * time.Second

// detectAccountTier 探测单个账号的层级。
//   - 付费模型探测成功        → pass
//   - 403 ENTITLEMENT        → free
//   - 429 限流               → 退避后重试一次（限流不代表层级）
//   - 其它错误（网络等）      → 保持 unknown，不改判定（避免把好号误标成 free）
func detectAccountTier(acc *Account) (string, error) {
	model := tierProbeModel()
	params := map[string]any{
		"model":      model,
		"max_tokens": minUpstreamMaxTokens,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, _, err := callClineAPIWithAccountCtx(context.Background(), acc, params, false)
		if err == nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			setAccountTier(acc, tierPass)
			return tierPass, nil
		}
		if apiErr, ok := err.(*clineAPIError); ok {
			if apiErr.entitlement {
				setAccountTier(acc, tierFree)
				return tierFree, nil
			}
			// 429：限流不是层级信息，退避后重试一次
			if apiErr.statusCode == http.StatusTooManyRequests && attempt == 0 {
				time.Sleep(tierProbeRetryWait)
				continue
			}
		}
		return tierUnknown, err
	}
	return tierUnknown, nil
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

		// 被显式指定过专供模型的账号优先探测：它们的层级直接决定付费模型能否工作。
		sort.SliceStable(targets, func(i, j int) bool {
			return len(targets[i].AssignedModels) > len(targets[j].AssignedModels)
		})

		tierDetectMu.Lock()
		tierDetectState = struct {
			Total     int       `json:"total"`
			Done      int       `json:"done"`
			Pass      int       `json:"pass"`
			Free      int       `json:"free"`
			Unknown   int       `json:"unknown"`
			StartedAt time.Time `json:"startedAt"`
			Finished  bool      `json:"finished"`
		}{Total: len(targets), StartedAt: time.Now()}
		tierDetectMu.Unlock()

		log.Printf("tier detect: probing %d account(s), probe model=%s", len(targets), tierProbeModel())

		// 串行探测 + 固定间隔：并发/连打过密会被上游 429 限流，
		// 反而让大量账号测不出层级（unknown）。
		for i, acc := range targets {
			if i > 0 {
				time.Sleep(tierProbeInterval)
			}
			tier, err := detectAccountTier(acc)
			tierDetectMu.Lock()
			tierDetectState.Done++
			switch tier {
			case tierPass:
				tierDetectState.Pass++
			case tierFree:
				tierDetectState.Free++
			default:
				tierDetectState.Unknown++
			}
			tierDetectMu.Unlock()
			if err != nil && tier == tierUnknown {
				log.Printf("tier detect: %s unknown (%v)", truncateEmail(acc.Email), err)
			}
			// 每 25 个落一次盘：探测可能持续很久，中途中断不至于全丢
			if i%25 == 24 {
				savePool()
			}
		}
		savePool()
		tierDetectMu.Lock()
		pass, free, unk := tierDetectState.Pass, tierDetectState.Free, tierDetectState.Unknown
		tierDetectMu.Unlock()
		log.Printf("tier detect done: pass=%d free=%d unknown=%d", pass, free, unk)
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
