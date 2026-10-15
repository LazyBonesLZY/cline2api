package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	pool     *AccountPool
	poolMu   sync.Mutex
	poolPath string
)

func init() {
	poolPath = resolveDataPath(".cline-accounts.json")
}

// dataDirEnv 数据目录环境变量。设置后所有数据文件固定存放在该目录（最高优先级），
// Docker 部署依赖它把数据落到挂载卷（Dockerfile 中 ENV CLINE2API_DATA_DIR=/app/data），
// 否则数据会写进容器可写层、重建容器即丢失。
const dataDirEnv = "CLINE2API_DATA_DIR"

// knownDataFiles 全部已知数据文件名，切换数据目录时用于旧数据的一次性迁移。
var knownDataFiles = []string{
	".cline-accounts.json",
	".cline-config.json",
	".cline-providers.json",
	".cline-request-logs.json",
	".cline-zen.json",
	".cline-proxy.json",
	".cline-credentials.json",
}

// migrateOnce 保证旧数据迁移在进程生命周期内只执行一次。
var migrateOnce sync.Once

// envDataDir 返回环境变量指定的数据目录（未设置返回空串），并确保目录存在。
func envDataDir() string {
	dir := strings.TrimSpace(os.Getenv(dataDirEnv))
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("Failed to create data dir %s: %v", dir, err)
	}
	return dir
}

// resolveDataPath 解析数据文件路径：
//  1. 设置了 CLINE2API_DATA_DIR 时固定使用该目录（首次调用会把旧位置的已有数据迁移过来）；
//  2. 未设置时按优先级查找：exe 目录 → 工作目录 → 用户主目录。
//     找到则用该路径（兼容旧版本在项目根目录存储的文件）；
//     都找不到则回退到 exe 目录（首次运行在该位置创建）。
func resolveDataPath(filename string) string {
	// 0. 环境变量指定的数据目录（Docker 卷挂载点）
	if dir := envDataDir(); dir != "" {
		migrateOnce.Do(func() { migrateLegacyDataFiles(dir) })
		return filepath.Join(dir, filename)
	}
	// 1. exe 所在目录
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), filename)
		if fileExists(p) {
			return p
		}
	}
	// 2. 当前工作目录
	if pwd, err := os.Getwd(); err == nil {
		p := filepath.Join(pwd, filename)
		if fileExists(p) {
			return p
		}
	}
	// 3. 用户主目录下的 .cline2api/
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cline2api", filename)
		if fileExists(p) {
			return p
		}
	}
	// 回退：exe 目录（首次运行在此创建）
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), filename)
	}
	pwd, _ := os.Getwd()
	return filepath.Join(pwd, filename)
}

// currentDataDir 返回当前生效的数据目录（新文件将创建的位置），用于启动日志与排查。
func currentDataDir() string {
	if dir := envDataDir(); dir != "" {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	pwd, _ := os.Getwd()
	return pwd
}

// legacyDataDirsOverride 仅供测试替换旧目录探测逻辑（nil 时使用默认探测）。
var legacyDataDirsOverride func() []string

// legacyDataCandidates 返回旧版本可能存放数据文件的目录（去重后的绝对路径）。
func legacyDataCandidates() []string {
	if legacyDataDirsOverride != nil {
		return legacyDataDirsOverride()
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if pwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, pwd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".cline2api"))
	}
	seen := make(map[string]bool, len(dirs))
	out := dirs[:0]
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			abs = d
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out
}

// migrateLegacyDataFiles 把旧位置已有的数据文件拷贝到数据目录（仅当目标不存在时）。
// 设置 CLINE2API_DATA_DIR 后升级部署时自动接管历史数据；幂等，可重复调用。
func migrateLegacyDataFiles(dir string) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("Failed to create data dir %s for migration: %v", dir, err)
		return
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	for _, name := range knownDataFiles {
		dst := filepath.Join(dir, name)
		if fileExists(dst) {
			continue
		}
		for _, d := range legacyDataCandidates() {
			if d == absDir {
				continue
			}
			src := filepath.Join(d, name)
			data, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			if err := writeFileAtomic(dst, data, 0600); err != nil {
				log.Printf("Failed to migrate %s from %s to %s: %v", name, d, dst, err)
				break
			}
			log.Printf("Migrated %s from %s to %s", name, d, dst)
			break
		}
	}
}

// writeFileAtomic 原子写文件：先写临时文件再 rename 覆盖。
// 直接 WriteFile 会截断原文件，进程写一半被杀（如 docker stop）会留下损坏的 JSON，
// 下次启动加载失败被当作空数据，再被空数据写回覆盖，造成数据永久丢失。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// 单文件 bind mount 上 rename 覆盖挂载点会返回 EBUSY：
		// 退回原地写，保证「单文件挂载」这种老部署方式仍能落盘。
		if writeErr := os.WriteFile(path, data, perm); writeErr != nil {
			return err
		}
		_ = os.Remove(tmp)
	}
	return nil
}

// backupCorruptFile 把解析失败的数据文件改名备份（.corrupt-<unix 秒>），
// 保留原始内容供人工恢复，避免后续保存把损坏文件静默覆盖。
func backupCorruptFile(path string, cause error) {
	backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if err := os.Rename(path, backup); err != nil {
		log.Printf("CORRUPT data file %s (%v); rename to backup failed: %v", path, cause, err)
		return
	}
	log.Printf("CORRUPT data file %s (%v); backed up to %s", path, cause, backup)
}

func loadPool() *AccountPool {
	poolMu.Lock()
	defer poolMu.Unlock()

	if pool != nil {
		return pool
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
		return pool
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		backupCorruptFile(poolPath, err)
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
		return pool
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	if p.Models == nil {
		p.Models = []Model{}
	}
	pool = &p
	return pool
}

// savePool 把当前账号池原子落盘。上游模型：本函数不加锁，
// 由调用方决定是否持有 poolMu（持锁时序列化是安全的）。
func savePool() {
	data, _ := json.MarshalIndent(pool, "", "  ")
	if err := writeFileAtomic(poolPath, data, 0600); err != nil {
		log.Printf("Failed to save accounts to %s: %v", poolPath, err)
	}
}

// addAccount 添加账号。同一邮箱（不区分大小写，来自 Cline userInfo，即同一
// Cline 账号）已存在时**原地更新凭据**：重新授权/重复导入不该产生重复条目 ——
// 保留原 AccountID、创建时间与用量统计，只更新令牌并置回 active。
// 已存在的同邮箱重复条目（历史遗留）一并合并进首个条目后移除（用量计数求和）。
func addAccount(acc *Account) {
	p := loadPool()
	poolMu.Lock()
	newEmail := strings.TrimSpace(acc.Email)
	var kept *Account
	var dups []*Account
	for _, a := range p.Accounts {
		if newEmail != "" && strings.EqualFold(strings.TrimSpace(a.Email), newEmail) {
			if kept == nil {
				kept = a
			} else {
				dups = append(dups, a)
			}
		}
	}
	if kept != nil {
		kept.RefreshToken = acc.RefreshToken
		kept.AccessToken = acc.AccessToken
		kept.ExpiresAt = acc.ExpiresAt
		kept.Status = "active"
		if len(dups) > 0 {
			// 合并历史重复条目：除了聚合计数，还要带上按模型统计与冷却，
			// 否则去重后「按模型用量」会凭空变小、冷却被提前解除。
			for _, d := range dups {
				mergeAccountStats(kept, d)
			}
			out := p.Accounts[:0]
			for _, a := range p.Accounts {
				isDup := false
				for _, d := range dups {
					if a == d {
						isDup = true
						break
					}
				}
				if !isDup {
					out = append(out, a)
				}
			}
			p.Accounts = out
			log.Printf("Account %s: merged %d duplicate entr(y/ies), stats preserved", acc.Email, len(dups))
		}
		log.Printf("Account re-authorized in place: %s (AccountID/stats preserved)", acc.Email)
	} else {
		p.Accounts = append(p.Accounts, acc)
	}
	poolMu.Unlock()
	savePool()
}

// findAccountByRefreshToken 按 refreshToken 查找已有账号（不存在返回 nil）。
// 用于导入时的去重：同一个 refreshToken 只应存在一个账号。
func findAccountByRefreshToken(refreshToken string) *Account {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil
	}
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if strings.TrimSpace(a.RefreshToken) == refreshToken {
			return a
		}
	}
	return nil
}

// accountExists 判断该 refreshToken 是否已在账号池中。
func accountExists(refreshToken string) bool {
	return findAccountByRefreshToken(refreshToken) != nil
}

// isDuplicateImportToken 判断待导入的 refreshToken 是否应跳过（导入去重）：
// 账号池中已存在同一 refreshToken，或本批次内已处理过（seen）。
// seen 由调用方维护、在此更新，用于同一批次内的去重。
func isDuplicateImportToken(refreshToken string, seen map[string]bool) bool {
	if seen[refreshToken] || accountExists(refreshToken) {
		return true
	}
	seen[refreshToken] = true
	return false
}

// normalizeEmail 归一化邮箱用于比对：去空白 + 转小写。
// 同一邮箱大小写不同（Outlook 常见）必须视为同一账号，否则会重复入池。
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// findAccountByEmail 按邮箱查找已有账号（不存在或邮箱为空返回 nil）。
func findAccountByEmail(email string) *Account {
	email = normalizeEmail(email)
	if email == "" {
		return nil
	}
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if normalizeEmail(a.Email) == email {
			return a
		}
	}
	return nil
}

// importDedup 维护一次导入批次内的去重状态。
// refreshToken 与邮箱各一份：同一账号可能以不同 token 重复出现（token 轮换后重新导入），
// 仅按 token 去重会漏掉这类重复。
type importDedup struct {
	seenTokens map[string]bool
	seenEmails map[string]bool
}

func newImportDedup() *importDedup {
	return &importDedup{seenTokens: map[string]bool{}, seenEmails: map[string]bool{}}
}

// duplicate 判断该条导入是否应跳过：refreshToken 或邮箱任一命中（池中已有 / 本批次已见）即重复。
// 命中后会同时记入两个 seen 集合。
func (d *importDedup) duplicate(refreshToken, email string) bool {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return true
	}
	email = normalizeEmail(email)

	if d.seenTokens[refreshToken] || accountExists(refreshToken) {
		return true
	}
	// 邮箱为空时无法做邮箱级去重，退化为仅按 token 去重
	if email != "" && (d.seenEmails[email] || findAccountByEmail(email) != nil) {
		return true
	}

	d.seenTokens[refreshToken] = true
	if email != "" {
		d.seenEmails[email] = true
	}
	return false
}

// accountScore 为去重时的「保留优先级」打分：状态可用优先，其次用量高的优先。
// 目的：合并重复记录时保留真正在用的那条，而不是随机留下一条。
func accountScore(a *Account) int64 {
	var s int64
	switch a.Status {
	case "active":
		s = 2
	case "cooldown":
		s = 1
	}
	return s*1_000_000_000 + a.UsageCount
}

// dedupeAccountsByEmail 清理账号池中同邮箱的重复记录。
// 每个邮箱只保留一条（状态可用优先、用量高优先），其余删除；
// 被删记录的用量与 token 统计累加进保留的那条，避免历史统计凭空消失。
// 返回 (删除条数, 去重后账号总数)。
func dedupeAccountsByEmail() (removed int, remaining int) {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	// 邮箱为空 / 仅空白的记录不参与去重（无法判定是否同一账号）
	best := make(map[string]*Account, len(p.Accounts))
	kept := make([]*Account, 0, len(p.Accounts))

	for _, a := range p.Accounts {
		email := normalizeEmail(a.Email)
		if email == "" {
			kept = append(kept, a)
			continue
		}
		prev, ok := best[email]
		if !ok {
			best[email] = a
			kept = append(kept, a)
			continue
		}
		// 重复记录：把统计并入分高的那条，然后丢弃当前这条
		keep, drop := prev, a
		if accountScore(a) > accountScore(prev) {
			keep, drop = a, prev
			// 替换 kept 中的旧条目，保持顺序稳定
			for i, k := range kept {
				if k == prev {
					kept[i] = a
					break
				}
			}
			best[email] = a
		}
		mergeAccountStats(keep, drop)
		removed++
	}

	if removed == 0 {
		return 0, len(p.Accounts)
	}
	p.Accounts = kept
	savePool()
	return removed, len(p.Accounts)
}

// mergeAccountStats 把 drop 的累计用量并入 keep（同账号的重复记录，统计应累加）。
func mergeAccountStats(keep, drop *Account) {
	keep.UsageCount += drop.UsageCount
	keep.PromptTokens += drop.PromptTokens
	keep.CompletionTokens += drop.CompletionTokens
	keep.TotalTokens += drop.TotalTokens
	keep.CachedTokens += drop.CachedTokens

	if drop.LastUsed.After(keep.LastUsed) {
		keep.LastUsed = drop.LastUsed
	}
	if len(drop.ModelStats) > 0 {
		if keep.ModelStats == nil {
			keep.ModelStats = make(map[string]*ModelStat)
		}
		for mid, st := range drop.ModelStats {
			if st == nil {
				continue
			}
			cur := keep.ModelStats[mid]
			if cur == nil {
				cp := *st
				keep.ModelStats[mid] = &cp
				continue
			}
			cur.UsageCount += st.UsageCount
			cur.PromptTokens += st.PromptTokens
			cur.CompletionTokens += st.CompletionTokens
			cur.TotalTokens += st.TotalTokens
			cur.CachedTokens += st.CachedTokens
		}
	}
	if len(drop.ModelCooldowns) > 0 {
		if keep.ModelCooldowns == nil {
			keep.ModelCooldowns = make(map[string]time.Time)
		}
		for mid, until := range drop.ModelCooldowns {
			if cur, ok := keep.ModelCooldowns[mid]; !ok || until.After(cur) {
				keep.ModelCooldowns[mid] = until
			}
		}
	}
	// 专供模型取并集：任一条指定过的模型都保留，避免去重后路由行为变化
	if len(drop.AssignedModels) > 0 {
		merged := append([]string(nil), keep.AssignedModels...)
		for _, m := range drop.AssignedModels {
			if !containsModel(merged, m) {
				merged = append(merged, m)
			}
		}
		keep.AssignedModels = merged
	}
}

func removeAccount(accountID string) bool {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			savePool()
			return true
		}
	}
	return false
}

func getAccountByID(accountID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if a.AccountID == accountID {
			return a
		}
	}
	return nil
}

// 同一账号的刷新合并成一次上游调用，避免并发轮换 refreshToken 时互相作废。
type refreshFlight struct {
	done chan struct{}
	err  error
}

var (
	refreshFlightMu sync.Mutex
	refreshFlights  = map[string]*refreshFlight{}
)

func refreshAccountToken(acc *Account) error {
	if acc == nil {
		return fmt.Errorf("nil account")
	}
	id := acc.AccountID
	if id == "" {
		id = acc.RefreshToken
	}

	refreshFlightMu.Lock()
	if flight, ok := refreshFlights[id]; ok {
		refreshFlightMu.Unlock()
		<-flight.done
		return flight.err
	}
	flight := &refreshFlight{done: make(chan struct{})}
	refreshFlights[id] = flight
	refreshFlightMu.Unlock()

	flight.err = refreshAccountTokenNow(acc)
	close(flight.done)

	refreshFlightMu.Lock()
	delete(refreshFlights, id)
	refreshFlightMu.Unlock()
	return flight.err
}

func refreshAccountTokenNow(acc *Account) error {
	poolMu.Lock()
	refreshToken := acc.RefreshToken
	poolMu.Unlock()

	resp, err := refreshClineToken(refreshToken)
	poolMu.Lock()
	defer poolMu.Unlock()
	if err != nil {
		// 400/401/403 才是凭据失效。超时、断连、5xx 只短冷却，后台探活可以把它拉回来。
		if isRefreshRejected(err) {
			// cline 自家 refresh 端点拒绝 ≠ 账号真失效：refresh token 本质是
			// WorkOS token，走 WorkOS authenticate 端点仍可能自愈，不必重新授权。
			if wos := refreshViaWorkOS(acc.RefreshToken); wos != nil {
				applyWorkOSTokens(acc, wos)
				return nil
			}
			acc.Status = "expired"
			savePool()
			// 双通道都拒绝 = WorkOS 会话已被服务端终结（invalid_grant: Session
			// has already ended / token revoked），刷新救不回，必须重新授权
			return fmt.Errorf("refresh token rejected by both Cline and WorkOS (session ended or revoked) — account requires re-authorization: 管理页「导入账号」→ OAuth 登录")
		}
		// 瞬时故障（超时/断连/5xx）只短冷却，且不覆盖已 expired 的判定
		if acc.Status != "expired" {
			acc.Status = "cooldown"
			acc.CooldownUntil = time.Now().Add(5 * time.Minute)
		}
		savePool()
		return fmt.Errorf("token refresh failed (transient): %w", err)
	}

	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	acc.CooldownUntil = time.Time{}
	savePool()
	return nil
}

// applyWorkOSTokens 用 WorkOS 直接刷新到的令牌续命账号：优先回 cline
// /auth/register 换正式 cline 令牌；注册也失败时直接用 workos 前缀令牌
// （Cline 上游接受），轮换出的新 refresh token 一并落盘。
func applyWorkOSTokens(acc *Account, wos *authenticateResp) {
	if cline, err := registerWithCline(wos.AccessToken, wos.RefreshToken); err == nil && cline.Data.AccessToken != "" {
		acc.AccessToken = "workos:" + cline.Data.AccessToken
		if cline.Data.RefreshToken != "" {
			acc.RefreshToken = cline.Data.RefreshToken
		}
		acc.ExpiresAt = parseExpiry(cline.Data.ExpiresAt) - 60000
	} else {
		acc.AccessToken = "workos:" + wos.AccessToken
		if wos.RefreshToken != "" {
			acc.RefreshToken = wos.RefreshToken
		}
		acc.ExpiresAt = time.Now().Add(time.Hour).UnixMilli() - 60000
	}
	acc.Status = "active"
	savePool()
	log.Printf("  account %s: token refreshed via WorkOS fallback (no re-auth needed)", acc.Email)
}

func pickAccount() *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return pickAccountLocked(p)
}

// pickAccountForModel 按轮询/策略挑选一个「该模型未处于模型级冷却」的账号；
// 所有 active 账号对该模型都冷却时回退到普通 pickAccount（请求会得到模型级 429 提示）。
// 空模型名等同于 pickAccount。
func pickAccountForModel(model string) *Account {
	return pickAccountForModelWithFallback(model, true)
}

func pickAccountForModelStrict(model string) *Account {
	return pickAccountForModelWithFallback(model, false)
}

func pickAccountForModelWithFallback(model string, fallbackToActive bool) *Account {
	if model == "" {
		return pickAccount()
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	// 候选集先按「模型指定」收敛：付费模型只由指定账号服务，
	// 被指定过的账号也不再接手其它模型（详见 modelCandidatesLocked）。
	candidates := modelCandidatesLocked(p, model)
	if len(candidates) == 0 {
		return nil
	}

	active := make([]*Account, 0, len(candidates))
	for _, a := range candidates {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}

	// 该模型未冷却的账号列表
	eligible := make([]*Account, 0, len(active))
	for _, a := range active {
		until, cool := a.ModelCooldowns[model]
		if !cool || time.Now().After(until) {
			if cool {
				delete(a.ModelCooldowns, model)
			}
			eligible = append(eligible, a)
		}
	}

	if len(eligible) == 0 {
		if fallbackToActive {
			// 只回退到候选集内的账号：回退到没有订阅的账号只会换来一个 403
			return pickFromAccountsLocked(p, active)
		}
		return nil
	}

	acc := pickFromAccountsLocked(p, eligible)
	savePool()
	return acc
}

// pickFromAccountsLocked 在已持有 poolMu 的前提下，按配置策略从给定账号列表里选一个。
func pickFromAccountsLocked(p *AccountPool, list []*Account) *Account {
	if len(list) == 0 {
		return nil
	}
	cfg := getProxyConfig()
	switch cfg.Strategy {
	case "fill":
		return list[0]
	case "random":
		return list[time.Now().UnixNano()%int64(len(list))]
	default: // round_robin
		if p.CurrentIdx >= len(list) {
			p.CurrentIdx = 0
		}
		acc := list[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(list)
		return acc
	}
}

// normalizeAssignedModels 清洗模型指定列表：去空去重，保持用户输入顺序。
func normalizeAssignedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	out := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func containsModel(list []string, model string) bool {
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}

// modelOwnersLocked 返回所有把该模型列为专属的账号（不论状态），
// 用于稳定判定「这个模型是否已被指定」。
func modelOwnersLocked(p *AccountPool, model string) []*Account {
	var owners []*Account
	for _, a := range p.Accounts {
		if containsModel(a.AssignedModels, model) {
			owners = append(owners, a)
		}
	}
	return owners
}

// modelCandidatesLocked 返回可以服务该模型的账号候选集（调用方再按状态/冷却过滤）：
//  1. 有账号把该模型列为专属（AssignedModels 含该模型）→ 只有这些账号服务它；
//     这是付费模型的正确语义：没有订阅的账号请求它只会拿到 403 ENTITLEMENT；
//  2. 该模型无人认领 → 只有「未指定任何模型」的账号参与，被指定过的账号
//     只服务它自己的列表，避免付费额度被免费流量打光。
//
// 无任何指定配置时，候选集等于全部账号，行为与旧版本完全一致。
func modelCandidatesLocked(p *AccountPool, model string) []*Account {
	if model == "" {
		return p.Accounts
	}
	if owners := modelOwnersLocked(p, model); len(owners) > 0 {
		return owners
	}
	out := make([]*Account, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		if len(a.AssignedModels) == 0 {
			out = append(out, a)
		}
	}
	return out
}

// pickAccountForModelLeastUsed 在所有「模型未冷却」的候选账号中，选择该模型
// 历史用量最少的账号（并清掉已过期的冷却记录）。等量时按轮询索引取，保持原有
// 公平性；全部不可用返回 nil。供回退链上的非首选模型使用：流量应摊到较少
// 使用的账号上，而不是每次都砸在第一个可用账号。
// 候选集与 pickAccountForModel 一致：模型指定的优先/独占规则同样生效。
func pickAccountForModelLeastUsed(model string) *Account {
	if model == "" {
		return pickAccount()
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	var bestCount int64
	var tied []*Account
	for _, a := range modelCandidatesLocked(p, model) {
		if a.Status != "active" {
			continue
		}
		if until, cool := a.ModelCooldowns[model]; cool {
			if time.Now().After(until) {
				delete(a.ModelCooldowns, model)
			} else {
				continue // 该账号此模型冷却中
			}
		}
		var cnt int64
		if st, ok := a.ModelStats[model]; ok && st != nil {
			cnt = st.UsageCount
		}
		if tied == nil || cnt < bestCount {
			bestCount = cnt
			tied = []*Account{a}
			continue
		}
		if cnt == bestCount {
			tied = append(tied, a)
		}
	}
	if len(tied) == 0 {
		return nil
	}
	// 用量相同（含付费模型从未记过 ModelStats、以及一批并发还没写回用量）时轮询，
	// 避免回退流量总是打在账号列表的第一个上。
	if p.CurrentIdx < 0 || p.CurrentIdx >= len(tied) {
		p.CurrentIdx = 0
	}
	acc := tied[p.CurrentIdx]
	p.CurrentIdx = (p.CurrentIdx + 1) % len(tied)
	savePool()
	return acc
}

// sortModelsByAvailability 将回退链按「可用性优先」重排：
//  1. 有未冷却账号的模型在前；
//  2. 同组内按该模型的账号总用量升序——把流量摊到用得少的模型上，
//     避免每次都选第一个可用模型、把它的额度打到冷却。
//  3. 稳定排序：可用性与用量相同时保持管理员配置的优先级顺序。
func sortModelsByAvailability(chain []string) []string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	now := time.Now()
	type cand struct {
		model     string
		avail     bool
		minUsage  int64
		origOrder int
	}
	cs := make([]cand, 0, len(chain))
	for i, m := range chain {
		avail := false
		minUsage := int64(-1)
		for _, a := range modelCandidatesLocked(p, m) {
			if a.Status != "active" {
				continue
			}
			if until, cool := a.ModelCooldowns[m]; cool {
				if now.After(until) {
					delete(a.ModelCooldowns, m)
				} else {
					continue
				}
			}
			avail = true
			var cnt int64
			if st, ok := a.ModelStats[m]; ok {
				cnt = st.UsageCount
			}
			if minUsage < 0 || cnt < minUsage {
				minUsage = cnt
			}
		}
		cs = append(cs, cand{model: m, avail: avail, minUsage: minUsage, origOrder: i})
	}
	sort.SliceStable(cs, func(x, y int) bool {
		if cs[x].avail != cs[y].avail {
			return cs[x].avail
		}
		if cs[x].avail && cs[x].minUsage != cs[y].minUsage {
			return cs[x].minUsage < cs[y].minUsage
		}
		return cs[x].origOrder < cs[y].origOrder
	})
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.model
	}
	savePool()
	return out
}

// pickAccountLocked 在已持有 poolMu 的前提下执行普通轮询挑选（供 pickAccountForModel 回退用）。
func pickAccountLocked(p *AccountPool) *Account {
	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}
	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = active[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default:
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}
	savePool()
	return acc
}

func ensureAccountToken(acc *Account) (string, error) {
	poolMu.Lock()
	token := acc.AccessToken
	exp := acc.ExpiresAt
	poolMu.Unlock()
	if token != "" && time.Now().UnixMilli() < exp {
		return token, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	poolMu.Lock()
	token = acc.AccessToken
	poolMu.Unlock()
	return token, nil
}

func listAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	result := make([]*Account, len(p.Accounts))
	for i, a := range p.Accounts {
		// Don't expose tokens
		cp := &Account{
			AccountID:        a.AccountID,
			Email:            a.Email,
			Status:           a.Status,
			CooldownUntil:    a.CooldownUntil,
			LastUsed:         a.LastUsed,
			UsageCount:       a.UsageCount,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			TotalTokens:      a.TotalTokens,
			CachedTokens:     a.CachedTokens,
			CreatedAt:        a.CreatedAt,
		}
		// 按模型细分统计（脱敏拷贝）
		if len(a.ModelStats) > 0 {
			cp.ModelStats = make(map[string]*ModelStat, len(a.ModelStats))
			for mid, st := range a.ModelStats {
				sc := *st
				cp.ModelStats[mid] = &sc
			}
		}
		// 模型级冷却（脱敏拷贝）
		if len(a.ModelCooldowns) > 0 {
			cp.ModelCooldowns = make(map[string]time.Time, len(a.ModelCooldowns))
			for mid, until := range a.ModelCooldowns {
				cp.ModelCooldowns[mid] = until
			}
		}
		// 模型指定（脱敏拷贝，供后台展示与编辑）
		if len(a.AssignedModels) > 0 {
			cp.AssignedModels = append([]string(nil), a.AssignedModels...)
		}
		result[i] = cp
	}
	return result
}

func addAccountFromDeviceAuth() (*Account, error) {
	fmt.Println()
	fmt.Println("=== Add New Cline Account (OAuth) ===")
	fmt.Println()

	device, err := workosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Println("  3. Log in with Google, GitHub, or email")
	fmt.Println()

	_ = openBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
	if err != nil {
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if cline.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
	}

	email := "unknown"
	if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
		email = cline.Data.UserInfo.Email
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        email,
		RefreshToken: cline.Data.RefreshToken,
		AccessToken:  "workos:" + cline.Data.AccessToken,
		ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}

	addAccount(acc)
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}
