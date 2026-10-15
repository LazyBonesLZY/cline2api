package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNormalizeTier 层级值归一化。
func TestNormalizeTier(t *testing.T) {
	cases := map[string]string{
		"":         tierUnknown,
		"  ":       tierUnknown,
		"pass":     tierPass,
		"PASS":     tierPass,
		" Pass ":   tierPass,
		"free":     tierFree,
		"FREE":     tierFree,
		"whatever": tierUnknown,
	}
	for in, want := range cases {
		if got := normalizeTier(in); got != want {
			t.Errorf("normalizeTier(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsPaidModelLocked 付费模型判定：查目录，目录缺失时按 cline-pass 前缀兜底。
func TestIsPaidModelLocked(t *testing.T) {
	p := &AccountPool{Models: []Model{
		{ID: "cline-pass/deepseek-v4.1-flash", Cost: "pass"},
		{ID: "cline-free/deepseek-v4.1-flash", Cost: "free"},
		{ID: "z-ai/glm-5.3-flash", Cost: "free"},
	}}
	cases := map[string]bool{
		"cline-pass/deepseek-v4.1-flash": true,
		"cline-free/deepseek-v4.1-flash": false,
		"z-ai/glm-5.3-flash":             false,
		"cline-pass/not-in-catalog":      true, // 目录里没有 → 前缀兜底
		"some/other-model":               false,
	}
	for model, want := range cases {
		if got := isPaidModelLocked(p, model); got != want {
			t.Errorf("isPaidModelLocked(%q) = %v, want %v", model, got, want)
		}
	}
}

// TestPaidModelCandidatesSkipKnownFree 付费模型的候选集必须排除已知 free 账号，
// 否则每个免费号都会被撞一次 403（这就是整池被毒化的根因）。
func TestPaidModelCandidatesSkipKnownFree(t *testing.T) {
	paid := &Account{AccountID: "paid", Email: "p@example.com", Status: "active", Tier: tierPass}
	free := &Account{AccountID: "free", Email: "f@example.com", Status: "active", Tier: tierFree}
	unknown := &Account{AccountID: "unk", Email: "u@example.com", Status: "active"}

	p := &AccountPool{
		Accounts: []*Account{paid, free, unknown},
		Models:   []Model{{ID: "cline-pass/m", Cost: "pass"}},
	}

	got := modelCandidatesLocked(p, "cline-pass/m")
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.AccountID] = true
	}
	if ids["free"] {
		t.Fatal("known-free account must be excluded from a paid model's candidates")
	}
	if !ids["paid"] {
		t.Fatal("pass account must stay in the candidate set")
	}
	if !ids["unk"] {
		t.Fatal("undetected account must stay in the candidate set (self-learning)")
	}
}

// TestFreeModelCandidatesKeepEveryone 免费模型不受层级限制：免费号也要能跑。
func TestFreeModelCandidatesKeepEveryone(t *testing.T) {
	paid := &Account{AccountID: "paid", Email: "p@example.com", Status: "active", Tier: tierPass}
	free := &Account{AccountID: "free", Email: "f@example.com", Status: "active", Tier: tierFree}

	p := &AccountPool{
		Accounts: []*Account{paid, free},
		Models:   []Model{{ID: "z-ai/glm-5.3-flash", Cost: "free"}},
	}

	got := modelCandidatesLocked(p, "z-ai/glm-5.3-flash")
	if len(got) != 2 {
		t.Fatalf("free model candidates = %d, want both accounts", len(got))
	}
}

// TestEntitlementDenialMarksAccountFree 403 ENTITLEMENT 应把账号标记为 free（自我学习），
// 后续付费模型不再选它。
func TestEntitlementDenialMarksAccountFree(t *testing.T) {
	withoutZen(t)
	oldTransport := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = oldTransport })

	acc := &Account{
		AccountID: "acc-403", Email: "no-sub@example.com", AccessToken: "workos:t",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}
	setAccounts(t, acc)
	setProxyConfig(defaultProxyConfig())

	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"code":"ENTITLEMENT_ERROR","message":"the user is not subscribed to required model plan"}}`)),
			Header:  make(http.Header),
			Request: req,
		}, nil
	})

	_, _, err := callClineAPI(map[string]any{"model": "cline-pass/deepseek-v4.1-flash"}, false)
	if err == nil {
		t.Fatal("expected an error for an unsubscribed account")
	}
	if got := accountTier(acc); got != tierFree {
		t.Fatalf("tier = %q, want %q after an entitlement denial", got, tierFree)
	}
	if acc.TierCheckedAt.IsZero() {
		t.Fatal("TierCheckedAt should be stamped")
	}
}

// TestTierCounts 层级统计（供后台筛选展示）。
func TestTierCounts(t *testing.T) {
	setAccounts(t,
		&Account{AccountID: "a", Email: "a@example.com", Status: "active", Tier: tierPass},
		&Account{AccountID: "b", Email: "b@example.com", Status: "active", Tier: tierFree},
		&Account{AccountID: "c", Email: "c@example.com", Status: "active", Tier: tierFree},
		&Account{AccountID: "d", Email: "d@example.com", Status: "active"},
	)
	c := tierCounts()
	if c[tierPass] != 1 || c[tierFree] != 2 || c["unknown"] != 1 {
		t.Fatalf("tierCounts = %+v, want pass=1 free=2 unknown=1", c)
	}
}

// TestAdminKeyUpdate 后台改密钥：就地替换、拒空、拒重复、找不到返回 404。
func TestAdminKeyUpdate(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{"k-old", "k-keep"}}

	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/keys/update", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleAdminKeyUpdate(w, req)
		return w.Code
	}

	if code := post(`{"old":"k-old","new":"k-new"}`); code != http.StatusOK {
		t.Fatalf("update status = %d, want 200", code)
	}
	if pool.Keys[0] != "k-new" || pool.Keys[1] != "k-keep" {
		t.Fatalf("keys = %v, want [k-new k-keep] (in-place, order kept)", pool.Keys)
	}
	if code := post(`{"old":"k-new","new":"k-keep"}`); code != http.StatusConflict {
		t.Fatalf("duplicate target status = %d, want 409", code)
	}
	if code := post(`{"old":"k-new","new":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("empty new value status = %d, want 400", code)
	}
	if code := post(`{"old":"nope","new":"x"}`); code != http.StatusNotFound {
		t.Fatalf("missing key status = %d, want 404", code)
	}
	if len(pool.Keys) != 2 {
		t.Fatalf("keys length changed: %v", pool.Keys)
	}
}
