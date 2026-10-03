package main

import (
	"testing"
	"time"
)

// TestNormalizeEmail 邮箱归一化：大小写与空白必须视为同一账号。
func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  A@B.com ":       "a@b.com",
		"User@Outlook.COM": "user@outlook.com",
		"":                 "",
		"   ":              "",
	}
	for in, want := range cases {
		if got := normalizeEmail(in); got != want {
			t.Errorf("normalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestImportDedupByEmail 同一邮箱换 token 重新导入必须判为重复（这是重复入池的根因）。
func TestImportDedupByEmail(t *testing.T) {
	setAccounts(t, &Account{
		AccountID:    "acc_a",
		Email:        "Dup@Example.com",
		RefreshToken: "token-old",
		Status:       "active",
	})

	d := newImportDedup()

	// 池中已有该邮箱（大小写不同）→ 重复
	if !d.duplicate("token-brand-new", "dup@example.com") {
		t.Fatal("same email with a rotated token must be a duplicate")
	}
	// 池中已有该 token → 重复
	if !d.duplicate("token-old", "other@example.com") {
		t.Fatal("same refresh token must be a duplicate")
	}
	// 全新邮箱 + 全新 token → 不重复
	if d.duplicate("token-fresh", "fresh@example.com") {
		t.Fatal("unseen email + unseen token must not be a duplicate")
	}
	// 同一批次内再次出现 → 重复
	if !d.duplicate("token-fresh", "fresh@example.com") {
		t.Fatal("second occurrence within the batch must be a duplicate")
	}
	// 空 token 永远跳过
	if !d.duplicate("", "fresh2@example.com") {
		t.Fatal("empty token must be skipped")
	}
	// 邮箱为空时退化为仅按 token 去重（不应误判）
	if d.duplicate("token-无邮箱", "") {
		t.Fatal("empty email should fall back to token-only dedup, not mark duplicate")
	}
}

// TestDedupeAccountsByEmailKeepsBestAndMergesStats 去重保留「状态可用 + 用量高」的那条，
// 并把被删记录的统计累加进去，历史用量不能凭空消失。
func TestDedupeAccountsByEmailKeepsBestAndMergesStats(t *testing.T) {
	weak := &Account{
		AccountID: "acc_weak", Email: "dup@example.com", RefreshToken: "rt-weak",
		Status: "expired", UsageCount: 3,
		PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CachedTokens: 5,
		ModelStats: map[string]*ModelStat{
			"m1": {ModelID: "m1", UsageCount: 3, TotalTokens: 30},
		},
	}
	strong := &Account{
		AccountID: "acc_strong", Email: "DUP@example.com", RefreshToken: "rt-strong",
		Status: "active", UsageCount: 100,
		PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, CachedTokens: 50,
		ModelStats: map[string]*ModelStat{
			"m1": {ModelID: "m1", UsageCount: 100, TotalTokens: 300},
			"m2": {ModelID: "m2", UsageCount: 7, TotalTokens: 70},
		},
	}
	other := &Account{AccountID: "acc_other", Email: "solo@example.com", Status: "active"}

	setAccounts(t, weak, strong, other)

	removed, remaining := dedupeAccountsByEmail()
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2", remaining)
	}

	kept := loadPool().Accounts
	var merged *Account
	for _, a := range kept {
		if normalizeEmail(a.Email) == "dup@example.com" {
			merged = a
		}
	}
	if merged == nil {
		t.Fatal("kept record missing")
	}
	if merged.AccountID != "acc_strong" {
		t.Fatalf("kept account = %q, want acc_strong (active + higher usage)", merged.AccountID)
	}
	if merged.UsageCount != 103 {
		t.Fatalf("UsageCount = %d, want 103 (100+3 merged)", merged.UsageCount)
	}
	if merged.PromptTokens != 110 || merged.CompletionTokens != 220 || merged.TotalTokens != 330 {
		t.Fatalf("token stats not merged: %+v", merged)
	}
	if merged.CachedTokens != 55 {
		t.Fatalf("CachedTokens = %d, want 55", merged.CachedTokens)
	}
	if st := merged.ModelStats["m1"]; st == nil || st.UsageCount != 103 || st.TotalTokens != 330 {
		t.Fatalf("per-model stats not merged: %+v", st)
	}
	if st := merged.ModelStats["m2"]; st == nil || st.UsageCount != 7 {
		t.Fatalf("model stats from the kept record lost: %+v", st)
	}
}

// TestDedupeMergesAssignedModels 去重后专供模型取并集，避免路由行为变化。
func TestDedupeMergesAssignedModels(t *testing.T) {
	a := &Account{
		AccountID: "acc_1", Email: "p@example.com", Status: "active", UsageCount: 5,
		AssignedModels: []string{"model-x"},
	}
	b := &Account{
		AccountID: "acc_2", Email: "P@example.com", Status: "active", UsageCount: 9,
		AssignedModels: []string{"model-y", "model-x"},
	}
	setAccounts(t, a, b)

	removed, _ := dedupeAccountsByEmail()
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	got := loadPool().Accounts[0]
	if got.AccountID != "acc_2" {
		t.Fatalf("kept = %q, want acc_2 (higher usage)", got.AccountID)
	}
	// 并集：model-x 与 model-y 都要在
	if len(got.AssignedModels) != 2 {
		t.Fatalf("AssignedModels = %v, want a 2-model union", got.AssignedModels)
	}
	if !containsModel(got.AssignedModels, "model-x") || !containsModel(got.AssignedModels, "model-y") {
		t.Fatalf("AssignedModels union wrong: %v", got.AssignedModels)
	}
}

// TestDedupeMergesCooldowns 冷却取较晚的截止时间，避免去重后提前恢复。
func TestDedupeMergesCooldowns(t *testing.T) {
	early := time.Now().Add(time.Hour)
	late := time.Now().Add(10 * time.Hour)

	a := &Account{
		AccountID: "acc_e", Email: "c@example.com", Status: "cooldown", UsageCount: 1,
		ModelCooldowns: map[string]time.Time{"m": early},
	}
	b := &Account{
		AccountID: "acc_l", Email: "C@example.com", Status: "active", UsageCount: 2,
		ModelCooldowns: map[string]time.Time{"m": late},
	}
	setAccounts(t, a, b)

	dedupeAccountsByEmail()
	got := loadPool().Accounts[0]
	if got.AccountID != "acc_l" {
		t.Fatalf("kept = %q, want acc_l", got.AccountID)
	}
	if until := got.ModelCooldowns["m"]; !until.Equal(late) {
		t.Fatalf("cooldown = %v, want the later deadline %v", until, late)
	}
}

// TestDedupeNoDuplicatesIsNoop 无重复时不应改动账号池。
func TestDedupeNoDuplicatesIsNoop(t *testing.T) {
	setAccounts(t,
		&Account{AccountID: "a", Email: "a@example.com", Status: "active"},
		&Account{AccountID: "b", Email: "b@example.com", Status: "active"},
	)
	removed, remaining := dedupeAccountsByEmail()
	if removed != 0 || remaining != 2 {
		t.Fatalf("removed=%d remaining=%d, want 0/2", removed, remaining)
	}
}

// TestDedupeKeepsEmptyEmailAccounts 邮箱为空的记录不参与去重（无法判定是否同一账号）。
func TestDedupeKeepsEmptyEmailAccounts(t *testing.T) {
	setAccounts(t,
		&Account{AccountID: "e1", Email: "", Status: "active"},
		&Account{AccountID: "e2", Email: "", Status: "active"},
		&Account{AccountID: "e3", Email: "  ", Status: "active"},
	)
	removed, remaining := dedupeAccountsByEmail()
	if removed != 0 || remaining != 3 {
		t.Fatalf("removed=%d remaining=%d, want 0/3 (blank emails must not be merged)", removed, remaining)
	}
}

// TestFindAccountByEmail 邮箱查找忽略大小写与空白。
func TestFindAccountByEmail(t *testing.T) {
	setAccounts(t, &Account{
		AccountID: "acc_x", Email: "Find@Example.com", Status: "active",
	})
	if got := findAccountByEmail("  find@example.com "); got == nil || got.AccountID != "acc_x" {
		t.Fatalf("findAccountByEmail should match case-insensitively, got %+v", got)
	}
	if got := findAccountByEmail(""); got != nil {
		t.Fatal("empty email must never match")
	}
	if got := findAccountByEmail("nope@example.com"); got != nil {
		t.Fatal("unknown email must not match")
	}
}
