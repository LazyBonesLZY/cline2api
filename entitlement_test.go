package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 403 ENTITLEMENT（账号未订阅该模型）的识别与处置：
// 它是「账号选错了」，必须换账号/换模型，而不是把 403 当请求错误透传给客户端。

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

// setAccounts 用给定账号替换账号池，测试结束恢复。
func setAccounts(t *testing.T, accounts ...*Account) {
	t.Helper()
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: accounts}
}

// TestPaidModelWithNoPassAccountReportsEntitlement 整池都没有订阅时，
// 付费模型应立刻报「无人订阅」（403），而不是退化成「全部冷却」的 429。
func TestPaidModelWithNoPassAccountReportsEntitlement(t *testing.T) {
	withoutZen(t)
	// 关掉故障转移：本用例验证的是「链上没有可用候选时」的判定，
	// 开着故障转移会退到免费模型并成功（那也是正确行为，见下个用例）。
	zenConfigMu.Lock()
	oldCfg := zenConfig
	noFailover := defaultZenConfig()
	noFailover.Enabled = false
	noFailover.Failover = false
	zenConfig = noFailover
	zenConfigMu.Unlock()
	t.Cleanup(func() {
		zenConfigMu.Lock()
		zenConfig = oldCfg
		zenConfigMu.Unlock()
	})

	oldTransport := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = oldTransport })

	free := &Account{
		AccountID: "only-free", Email: "free@example.com", AccessToken: "workos:t",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", Tier: tierFree,
	}
	setAccounts(t, free)
	setProxyConfig(defaultProxyConfig())

	// 不该发出任何上游请求：候选集为空时就该判定
	calls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	_, _, err := callClineAPI(map[string]any{"model": "cline-pass/deepseek-v4.1-flash"}, false)
	if err == nil {
		t.Fatal("expected an error when no account is subscribed")
	}
	var entErr *modelEntitlementError
	if !asError(err, &entErr) {
		t.Fatalf("expected modelEntitlementError, got %T: %v", err, err)
	}
	if got := clineErrorHTTPStatus(err); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0 (no candidate → decide locally)", calls)
	}
}
