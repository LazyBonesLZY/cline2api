package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setAccounts 用给定账号替换账号池，测试结束恢复。
func setAccounts(t *testing.T, accounts ...*Account) {
	t.Helper()
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: accounts}
}

// TestAssignedModelExclusiveToOwner 付费模型只由被指定的账号服务：
// 未订阅的免费账号不应被选中（否则就是 403 ENTITLEMENT 的来源）。
func TestAssignedModelExclusiveToOwner(t *testing.T) {
	paid := &Account{AccountID: "paid", Email: "paid@example.com", Status: "active",
		AssignedModels: []string{"cline-pass/deepseek-v4.1-flash"}}
	free := &Account{AccountID: "free", Email: "free@example.com", Status: "active"}

	setAccounts(t, paid, free)
	setProxyConfig(defaultProxyConfig())

	for i := 0; i < 6; i++ {
		got := pickAccountForModelStrict("cline-pass/deepseek-v4.1-flash")
		if got == nil {
			t.Fatal("expected the assigned paid account to be picked")
		}
		if got.AccountID != "paid" {
			t.Fatalf("paid model routed to unassigned account %q", got.AccountID)
		}
	}
}

// TestAssignedAccountExcludedFromOtherModels 被指定过模型的账号不再接手其它模型，
// 避免付费额度被免费模型流量打光。
func TestAssignedAccountExcludedFromOtherModels(t *testing.T) {
	paid := &Account{AccountID: "paid", Email: "paid@example.com", Status: "active",
		AssignedModels: []string{"cline-pass/deepseek-v4.1-flash"}}
	free := &Account{AccountID: "free", Email: "free@example.com", Status: "active"}

	setAccounts(t, paid, free)
	setProxyConfig(defaultProxyConfig())

	for i := 0; i < 6; i++ {
		got := pickAccountForModelStrict("some-other-model")
		if got == nil {
			t.Fatal("expected the unrestricted account to be picked")
		}
		if got.AccountID != "free" {
			t.Fatalf("unrestricted model routed to assigned account %q", got.AccountID)
		}
	}
}

// TestNoAssignmentKeepsLegacyBehavior 没有任何指定时，候选集是全部账号，
// 与旧版本行为完全一致（回归保护）。
func TestNoAssignmentKeepsLegacyBehavior(t *testing.T) {
	a := &Account{AccountID: "a", Email: "a@example.com", Status: "active"}
	b := &Account{AccountID: "b", Email: "b@example.com", Status: "active"}

	setAccounts(t, a, b)
	setProxyConfig(defaultProxyConfig())

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got := pickAccountForModelStrict("z-ai/glm-5.3-flash")
		if got == nil {
			t.Fatal("expected an account")
		}
		seen[got.AccountID] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("round robin should rotate over all accounts, saw %v", seen)
	}
}

// TestAssignedModelLeastUsedStaysInCandidateSet 降级链的「最久未用优先」
// 也必须遵守候选集，否则回退流量会落到未订阅的账号上。
func TestAssignedModelLeastUsedStaysInCandidateSet(t *testing.T) {
	paid := &Account{AccountID: "paid", Email: "paid@example.com", Status: "active",
		AssignedModels: []string{"paid/model"}}
	free := &Account{AccountID: "free", Email: "free@example.com", Status: "active"}

	setAccounts(t, paid, free)
	setProxyConfig(defaultProxyConfig())

	got := pickAccountForModelLeastUsed("paid/model")
	if got == nil || got.AccountID != "paid" {
		t.Fatalf("least-used picker escaped the candidate set: %+v", got)
	}
}

// TestIsEntitlementError 识别 Cline 的未订阅响应（这是本次报错的根因）。
func TestIsEntitlementError(t *testing.T) {
	body := `{"error":{"code":"ENTITLEMENT_ERROR","message":"Error 403: the user is not subscribed to required model plan"}}`
	if !isEntitlementError(http.StatusForbidden, body) {
		t.Fatal("ENTITLEMENT_ERROR 403 should be detected")
	}
	if isEntitlementError(http.StatusBadRequest, body) {
		t.Fatal("400 must not be treated as an entitlement denial")
	}
	if isEntitlementError(http.StatusForbidden, `{"error":"plain forbidden"}`) {
		t.Fatal("unrelated 403 must not be treated as an entitlement denial")
	}
}

// TestEntitlementDenialFallsOverToOtherAccount 未订阅的账号被拒后应换号，
// 而不是把 403 当成客户端错误透传（原 bug：最终变成 500）。
func TestEntitlementDenialFallsOverToOtherAccount(t *testing.T) {
	withoutZen(t)
	oldTransport := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = oldTransport })

	// 两个账号都在同一条链上；第一个未订阅并返回 403，第二个可用。
	blocked := &Account{AccountID: "blocked", Email: "blocked@example.com",
		AccessToken: "workos:blocked", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	good := &Account{AccountID: "good", Email: "good@example.com",
		AccessToken: "workos:good", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	setAccounts(t, blocked, good)
	setProxyConfig(defaultProxyConfig())

	cfg := getZenConfig()
	if !cfg.Failover {
		t.Skip("fallback disabled in this environment")
	}

	var tokens []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, token)
		if token == "workos:blocked" {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"code":"ENTITLEMENT_ERROR","message":"the user is not subscribed to required model plan"}}`)),
				Header:  make(http.Header),
				Request: req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
			Header:  make(http.Header),
			Request: req,
		}, nil
	})

	resp, acc, err := callClineAPI(map[string]any{"model": "cline-pass/deepseek-v4.1-flash"}, false)
	if err != nil {
		t.Fatalf("entitlement denial should fail over, got error: %v", err)
	}
	defer resp.Body.Close()
	if acc == nil || acc.AccountID != "good" {
		t.Fatalf("expected failover to the good account, got %+v", acc)
	}
	if len(tokens) < 2 {
		t.Fatalf("expected a retry on another account, tokens=%v", tokens)
	}
	// 被拒的账号应被记上该模型的冷却，避免后续每次请求都先撞一次 403
	if _, cool := blocked.ModelCooldowns["cline-pass/deepseek-v4.1-flash"]; !cool {
		t.Fatal("denied account should be cooled down for that model")
	}
}

// TestEntitlementExhaustionReturns403 所有候选都被拒时返回 403（可操作的错误），
// 而不是 500。
func TestEntitlementExhaustionReturns403(t *testing.T) {
	withoutZen(t)
	oldTransport := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = oldTransport })

	only := &Account{AccountID: "only", Email: "only@example.com",
		AccessToken: "workos:only", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	setAccounts(t, only)
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
		t.Fatal("expected an error when every account denies entitlement")
	}
	var entErr *modelEntitlementError
	if !asError(err, &entErr) {
		t.Fatalf("expected modelEntitlementError, got %T: %v", err, err)
	}
	if got := clineErrorHTTPStatus(err); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
}

// asError 是 errors.As 的薄包装，避免在测试里重复 import errors。
func asError(err error, target **modelEntitlementError) bool {
	e, ok := err.(*modelEntitlementError)
	if ok {
		*target = e
	}
	return ok
}

// TestAdminAccountModelsAPI 覆盖后台指定接口：保存、回读、校验、解除。
func TestAdminAccountModelsAPI(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })

	acc := &Account{AccountID: "acc_api", Email: "api@example.com", Status: "active"}
	pool = &AccountPool{Accounts: []*Account{acc}, Models: []Model{
		{ID: "paid/model", Cost: "pass", Status: "active"},
		{ID: "free/model", Cost: "free", Status: "active"},
	}}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/models", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleAdminAccountModels(w, req)
		return w
	}

	// 保存指定
	w := post(`{"accountId":"acc_api","models":["paid/model"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(acc.AssignedModels) != 1 || acc.AssignedModels[0] != "paid/model" {
		t.Fatalf("assigned models = %v, want [paid/model]", acc.AssignedModels)
	}

	// listAccounts 应把指定带回后台
	got := listAccounts()
	if len(got) != 1 || len(got[0].AssignedModels) != 1 {
		t.Fatalf("listAccounts lost the assignment: %+v", got)
	}
	if got[0].RefreshToken != "" {
		t.Fatal("listAccounts must not leak refresh tokens")
	}

	// 去重 + 去空白
	w = post(`{"accountId":"acc_api","models":[" paid/model ","paid/model",""]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("dedupe save status = %d", w.Code)
	}
	if len(acc.AssignedModels) != 1 {
		t.Fatalf("expected dedupe to leave 1 model, got %v", acc.AssignedModels)
	}

	// 未知模型应被拒绝，且不破坏已有配置
	w = post(`{"accountId":"acc_api","models":["nope/not-real"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown model should be rejected, status = %d body = %s", w.Code, w.Body.String())
	}
	if len(acc.AssignedModels) != 1 || acc.AssignedModels[0] != "paid/model" {
		t.Fatalf("rejected save must not mutate state, got %v", acc.AssignedModels)
	}

	// 空数组 = 解除指定
	w = post(`{"accountId":"acc_api","models":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear status = %d", w.Code)
	}
	if len(acc.AssignedModels) != 0 {
		t.Fatalf("expected cleared assignment, got %v", acc.AssignedModels)
	}

	// 不存在的账号 → 404
	w = post(`{"accountId":"missing","models":["paid/model"]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", w.Code)
	}
}

// TestAssignedModelsSurviveReload 指定配置必须随账号池一起持久化（重启后仍生效）。
func TestAssignedModelsSurviveReload(t *testing.T) {
	oldPool, oldPath := pool, poolPath
	t.Cleanup(func() { pool, poolPath = oldPool, oldPath })

	dir := t.TempDir()
	poolPath = filepath.Join(dir, ".cline-accounts.json")
	pool = &AccountPool{Accounts: []*Account{{
		AccountID: "persist", Email: "p@example.com", Status: "active",
		AssignedModels: []string{"cline-pass/deepseek-v4.1-flash"},
	}}}
	savePool()

	pool = nil // 强制从磁盘重载
	reloaded := loadPool()
	if len(reloaded.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(reloaded.Accounts))
	}
	am := reloaded.Accounts[0].AssignedModels
	if len(am) != 1 || am[0] != "cline-pass/deepseek-v4.1-flash" {
		t.Fatalf("assignment did not survive reload: %v", am)
	}
}
