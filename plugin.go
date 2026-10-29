package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type app struct {
	mu     sync.Mutex
	cfg    Config
	rawCfg string
	// rawYAML is the plugin config block as the host sent it, kept so the console
	// can drop its saved overrides and fall back to it without a restart.
	rawYAML []byte
	state   *stateStore
	up      *upstream
	stop    context.CancelFunc
	done    chan struct{}
	started bool
	// closing stops background goroutines from calling back into a host that is
	// already shutting the plugin down.
	closing atomic.Bool
	// dirty flags account state (health, tokens) and the points cache for the
	// periodic flush; the rings carry their own flags.
	dirty       atomic.Bool
	pointsDirty atomic.Bool
	// bg tracks goroutines that may still touch the host.
	bg sync.WaitGroup

	pool     *pool
	accounts *accountManager

	log      *logRing
	metrics  *metrics
	requests *requestLog

	saveMu sync.Mutex

	catalogMu sync.Mutex
	catalog   catalogSnapshot

	publishMu sync.Mutex
	published map[string]string
	// publishGate throttles auth-record writes: a save makes the host reload its
	// auth directory, so a runaway writer would re-enter the plugin.
	publishGate *rateGate

	pointsMu sync.Mutex
	points   map[string]*pointsCache
	// pointsQueue bounds the ledger backfill work: one worker, drop when full.
	pointsQueue chan pointsJob
	// expiry caches how close each account is to losing points, used to route
	// traffic onto balances that would otherwise lapse.
	expiryMu sync.RWMutex
	expiry   map[string]expiryScore

	loginMu sync.Mutex
	logins  map[string]*loginSession
}

var plugin = &app{
	pool:        newPool(),
	log:         newLogRing(600),
	metrics:     &metrics{},
	requests:    newRequestLog(300),
	published:   make(map[string]string),
	publishGate: newRateGate(12, 10*time.Second),
	points:      make(map[string]*pointsCache),
	expiry:      make(map[string]expiryScore),
	logins:      make(map[string]*loginSession),
}

func (a *app) currentConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *app) stateStore() *stateStore {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

func (a *app) upstream() *upstream {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.up
}

func (a *app) configure(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	// Console edits persist in <data_dir>/settings.json and outrank the file
	// config, so a restart does not silently undo what the user just saved.
	cfg = loadSavedSettings(cfg, a.log)
	// The host reconfigures on every auth-directory change; skip the expensive
	// work when nothing actually changed. The fingerprint covers every field so
	// a proxy or limit edit is never silently ignored.
	fingerprint := fmt.Sprintf("%+v", cfg)
	a.mu.Lock()
	if a.rawCfg == fingerprint && a.state != nil && a.up != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	a.mu.Lock()
	previous := a.cfg
	previousUp := a.up
	a.cfg = cfg
	a.up = newUpstream(cfg)
	if a.accounts == nil {
		a.accounts = newAccountManager(a)
	}
	a.pool.expiry = a.expiryOf
	if a.pointsQueue == nil {
		a.pointsQueue = make(chan pointsJob, 256)
	}
	if a.state == nil || previous.DataDir != cfg.DataDir {
		store, errStore := newStateStore(cfg.DataDir)
		if errStore != nil {
			a.mu.Unlock()
			return fmt.Errorf("init state store: %w", errStore)
		}
		a.state = store
	}
	// Only remember the config once every side effect succeeded, so a failed
	// configure is retried instead of being treated as a no-op.
	a.rawCfg = fingerprint
	a.rawYAML = append(a.rawYAML[:0], raw...)
	a.closing.Store(false)
	a.mu.Unlock()
	if previousUp != nil {
		previousUp.close()
	}

	a.pool.mu.Lock()
	loaded := len(a.pool.accounts) > 0
	a.pool.mu.Unlock()
	if !loaded {
		a.loadPool()
		a.loadRuntimeState()
	} else {
		a.scanAccountsDir(cfg)
	}
	for _, c := range a.pool.snapshot() {
		c.mu.Lock()
		if c.bucket == nil || previous.RPM != cfg.RPM || previous.Burst != cfg.Burst {
			c.bucket = newTokenBucket(cfg.RPM, cfg.Burst)
		}
		c.mu.Unlock()
	}

	// The host asks for models the moment an auth record is written, so the
	// catalogue has to exist before publishing.
	if len(a.snapshotCatalog().Models) == 0 {
		if cached, ok := a.loadCachedCatalog(); ok {
			a.setCatalog(cached)
		}
	}
	if len(a.snapshotCatalog().Models) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		if errCatalog := a.refreshCatalog(ctx); errCatalog != nil {
			a.log.warn("catalog", "首次模型发现失败："+errCatalog.Error())
		}
		cancel()
	}
	for _, account := range a.pool.snapshot() {
		a.publishAccount(account, false)
	}
	a.startBackground()
	return nil
}

// spawn runs fn on a tracked goroutine that is skipped once the plugin closes.
func (a *app) spawn(scope string, fn func()) {
	if a.closing.Load() {
		return
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		defer guard(a, scope)
		fn()
	}()
}

func (a *app) startBackground() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	a.done = make(chan struct{})
	a.started = true
	done := a.done
	a.mu.Unlock()

	a.spawn("maintenance", func() { a.maintenanceLoop(ctx) })
	a.spawn("points", func() { a.dailyPointsLoop(ctx) })
	a.spawn("expiry", func() { a.expiryLoop(ctx) })
	a.spawn("persist", func() { a.persistLoop(ctx) })
	a.spawn("points-queue", func() { a.pointsWorker(ctx) })
	go func() {
		<-ctx.Done()
		close(done)
	}()
}

func (a *app) shutdown() {
	a.closing.Store(true)
	a.mu.Lock()
	cancel := a.stop
	done := a.done
	a.stop = nil
	a.started = false
	a.done = nil
	up := a.up
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			a.log.warn("plugin", "后台任务未在 5 秒内退出，继续关闭")
		}
	}
	// Give tracked goroutines a moment; they check closing before calling back.
	waited := make(chan struct{})
	go func() {
		a.bg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		a.log.warn("plugin", "后台任务未在 5 秒内退出，继续关闭")
	}
	if up != nil {
		up.close()
	}
	// Last flush so the console keeps its history and the accounts keep their
	// cooldowns across the restart.
	a.flushState()
}

func (a *app) maintenanceLoop(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			cfg := a.currentConfig()
			a.maintainOnce(ctx)
			timer.Reset(time.Duration(cfg.importInterval()) * time.Second)
		}
	}
}

func (a *app) maintainOnce(ctx context.Context) {
	cfg := a.currentConfig()
	a.scanAccountsDir(cfg)
	a.refreshStaleAccounts(ctx)
	a.autoRelogin(ctx)
	a.ensureHostRecords()
	a.refreshMissingProfiles()

	snapshot := a.snapshotCatalog()
	if len(snapshot.Models) == 0 || time.Since(snapshot.FetchedAt) > time.Duration(cfg.RefreshSeconds)*time.Second {
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := a.refreshCatalog(refreshCtx); err != nil {
			a.log.warn("catalog", "模型目录刷新失败："+err.Error())
		}
		cancel()
	}
}

// refreshMissingProfiles fills in the console's user column one account per
// tick, so a restart does not leave every account without a nickname.
func (a *app) refreshMissingProfiles() {
	for _, account := range a.pool.snapshot() {
		account.mu.Lock()
		missing := account.Nickname == "" && account.Phone == ""
		account.mu.Unlock()
		if !missing {
			continue
		}
		a.spawn("profile", func() { a.refreshAccountInfo(account) })
		return
	}
}

// refreshStaleAccounts renews api keys that are about to expire, one account at
// a time so one bad credential cannot stall the loop.
func (a *app) refreshStaleAccounts(ctx context.Context) {
	for _, c := range a.pool.snapshot() {
		if !usable(c) || c.apiKeyValid(10*time.Minute) {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if _, err := a.accounts.ensureAPIKey(refreshCtx, c); err != nil {
			a.log.warn("accounts", fmt.Sprintf("账号 %s 令牌续期失败：%s", c.label(), asFailure(err).Message))
		}
		cancel()
	}
}

func (a *app) hostAuths() ([]hostAuthEntry, error) {
	if a.closing.Load() {
		return nil, errPluginClosing
	}
	raw, err := hostCall(methodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	out := make([]hostAuthEntry, 0, len(resp.Files))
	for _, entry := range resp.Files {
		if entry.Provider == providerKey || entry.Type == providerKey {
			out = append(out, entry)
		}
	}
	return out, nil
}

// ensureHostRecords recreates any pool account whose CPA credential vanished.
func (a *app) ensureHostRecords() {
	accounts := a.pool.snapshot()
	if len(accounts) == 0 {
		return
	}
	entries, err := a.hostAuths()
	if err != nil {
		return
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if id := authIDOf(entry); id != "" {
			present[id] = true
		}
	}
	for _, c := range accounts {
		if present[c.ID] {
			continue
		}
		a.publishAccount(c, true)
		a.log.info("accounts", "已把账号 "+c.label()+" 重新写入 CPA 凭据")
	}
}

// aliasFromAuthFileName recovers the account id from a credential file name of
// the form magic2api-<id>.json.
func aliasFromAuthFileName(name string) string {
	base := strings.TrimSuffix(strings.TrimSpace(name), ".json")
	prefix := providerKey + "-"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}
	return sanitizeAlias(strings.TrimPrefix(base, prefix))
}

func authIDOf(entry hostAuthEntry) string {
	name := entry.Name
	if name == "" {
		return ""
	}
	base := strings.TrimSuffix(name, ".json")
	base = strings.TrimPrefix(base, providerKey+"-")
	return base
}

func (a *app) registration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             "Magic2API (超级麦吉)",
			Version:          pluginVersion,
			Author:           "JustCnc",
			GitHubRepository: "https://github.com/JustCnc/magic2api",
			Logo:             "",
			ConfigFields:     pluginConfigFields(),
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{formatChat},
			ExecutorOutputFormats: []string{formatChat},
			ManagementAPI:         true,
		},
	}
}

func decodeRequest[T any](raw []byte) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode %T: %w", out, err)
	}
	return out, nil
}

func (a *app) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		lifecycle, err := decodeRequest[lifecycleRequest](request)
		if err != nil {
			return nil, err
		}
		if err := a.configure(lifecycle.ConfigYAML); err != nil {
			return errorEnvelope("config_error", err.Error()), nil
		}
		if method == methodPluginRegister {
			a.log.info("plugin", "插件已注册 v"+pluginVersion)
		} else {
			a.log.info("plugin", "配置已更新")
		}
		return okEnvelope(a.registration())
	case methodPluginQuiesce:
		return okEnvelope(map[string]any{})
	case methodPluginShutdown:
		a.shutdown()
		a.log.info("plugin", "插件已停止")
		return okEnvelope(map[string]any{})
	case methodModelRegister, methodModelStatic, methodModelForAuth:
		return okEnvelope(modelRegistrationResponse{Provider: providerKey, Models: a.registeredModels()})
	case methodAuthIdentifier, methodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case methodAuthParse:
		return a.parseAuth(request)
	case methodAuthLoginStart:
		return a.startLogin(request)
	case methodAuthLoginPoll:
		return a.pollLogin(request)
	case methodAuthRefresh:
		return a.refreshAuth(request)
	case methodExecutorExecute:
		return a.execute(request)
	case methodExecutorExecuteStream:
		return a.executeStream(request)
	case methodExecutorCountTokens:
		return okEnvelope(map[string]any{"Tokens": 0})
	case methodManagementRegister:
		return okEnvelope(a.managementRegistration())
	case methodManagementHandle:
		req, err := decodeRequest[managementRequest](request)
		if err != nil {
			return nil, err
		}
		return okEnvelope(a.handleManagement(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// publishAccount writes the account into a CPA auth record. The record body is
// the same JSON the console exports, so a round trip is lossless.
func (a *app) publishAccount(c *credential, force bool) {
	if c == nil || c.Authorization == "" || a.closing.Load() {
		return
	}
	if !a.publishGate.allow() {
		a.log.warn("accounts", "凭据写入过于频繁，已跳过本次")
		return
	}
	fingerprint := c.Authorization + "|" + c.OrganizationCode + "|" + c.Device.ID + "|" + fmt.Sprint(c.Disabled)
	a.publishMu.Lock()
	previous, seen := a.published[c.ID]
	if !force && seen && previous == fingerprint {
		a.publishMu.Unlock()
		return
	}
	a.published[c.ID] = fingerprint
	a.publishMu.Unlock()

	record := authRecordFor(c)
	raw, err := json.Marshal(record)
	if err != nil {
		a.log.warn("accounts", "序列化凭据失败："+err.Error())
		return
	}
	name := fmt.Sprintf("%s-%s.json", providerKey, c.ID)
	if _, errSave := hostCall(methodHostAuthSave, hostAuthSaveRequest{Name: name, JSON: raw}); errSave != nil {
		a.log.warn("accounts", "写入 CPA 凭据失败："+errSave.Error())
	}
}

func authRecordFor(c *credential) map[string]any {
	return map[string]any{
		"type":              providerKey,
		"provider":          providerKey,
		"alias":             c.Alias,
		"authorization":     c.Authorization,
		"organization_code": c.OrganizationCode,
		"device":            c.Device,
		"priority":          c.Priority,
		"note":              c.Note,
	}
}

func (a *app) authDataFor(c *credential, fileName string) authData {
	raw, _ := json.Marshal(authRecordFor(c))
	return authData{
		Provider:    providerKey,
		ID:          c.ID,
		FileName:    fileName,
		Label:       c.label(),
		StorageJSON: raw,
		Disabled:    c.Disabled,
		Metadata: map[string]any{
			"organization_code": c.OrganizationCode,
			"device_id":         c.Device.ID,
			"device_name":       c.Device.Name,
			"phone":             c.Phone,
			"nickname":          c.Nickname,
		},
		Attributes: map[string]string{
			"organization_code": c.OrganizationCode,
			"device_id":         c.Device.ID,
		},
		NextRefreshAfter: time.Now().Add(90 * time.Minute),
	}
}

func pluginConfigFields() []configField {
	return []configField{
		{Name: "authorization", Type: "string", Default: "", Description: "主账号的登录 JWT（与 organization_code 同时提供；也可只用控制台登录/导入）"},
		{Name: "organization_code", Type: "string", Default: "", Description: "主账号所属组织编码，例如 h70imn51gp0f"},
		{Name: "accounts_dir", Type: "string", Default: "", Description: "额外账号目录，每个 *.json 一个账号，文件名即别名"},
		{Name: "model_prefix", Type: "string", Default: "magic", Description: "对外暴露的模型前缀，magic/<上游模型 id>"},
		{Name: "rpm", Type: "int", Default: 30, Description: "单账号每分钟请求上限，超限返回本地 429 让宿主换号"},
		{Name: "burst", Type: "int", Default: 20, Description: "单账号令牌桶容量"},
		{Name: "cooldown_seconds", Type: "int", Default: 60, Description: "上游可重试错误后账号冷却秒数"},
		{Name: "request_timeout_seconds", Type: "int", Default: 300, Description: "单次上游请求超时"},
		{Name: "max_attempts", Type: "int", Default: 3, Description: "一次请求最多尝试几个账号"},
		{Name: "points_logging", Type: "bool", Default: true, Description: "请求结束后回查积分流水并回填消耗"},
		{Name: "daily_points_enabled", Type: "bool", Default: true, Description: "自动领取每日登录积分与新人任务奖励"},
		{Name: "proxy_url", Type: "string", Default: "", Description: "出站 HTTP 代理，例如 http://127.0.0.1:7897"},
		{Name: "debug_dump", Type: "bool", Default: false, Description: "在控制台日志里打印请求/响应片段"},
	}
}

// rateGate is a sliding-window limiter.
type rateGate struct {
	mu     sync.Mutex
	hits   []time.Time
	limit  int
	window time.Duration
}

func newRateGate(limit int, window time.Duration) *rateGate {
	return &rateGate{limit: limit, window: window}
}

func (g *rateGate) allow() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-g.window)
	kept := g.hits[:0]
	for _, hit := range g.hits {
		if hit.After(cutoff) {
			kept = append(kept, hit)
		}
	}
	g.hits = kept
	if len(g.hits) >= g.limit {
		return false
	}
	g.hits = append(g.hits, now)
	return true
}

// ---- auth RPC ----

func (a *app) parseAuth(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authParseRequest](raw)
	if err != nil {
		return nil, err
	}
	if len(req.RawJSON) == 0 {
		return okEnvelope(authParseResponse{Handled: false})
	}
	var in accountInput
	if json.Unmarshal(req.RawJSON, &in) != nil {
		return okEnvelope(authParseResponse{Handled: false})
	}
	untyped := strings.TrimSpace(in.Type) == "" && strings.TrimSpace(in.Provider) == ""
	ourProvider := strings.EqualFold(strings.TrimSpace(req.Provider), providerKey)
	if !untyped && in.Type != providerKey && in.Provider != providerKey && !ourProvider {
		return okEnvelope(authParseResponse{Handled: false})
	}
	if strings.TrimSpace(in.Authorization) == "" {
		return okEnvelope(authParseResponse{Handled: false})
	}
	// The file name carries the account id (magic2api-<id>.json); the alias inside
	// is a display name the user may edit. Keep them separate, otherwise a renamed
	// account comes back as a second account on the next parse.
	id := aliasFromAuthFileName(req.FileName)
	alias := sanitizeAlias(in.Alias)
	if id == "" {
		id = alias
	}
	if alias == "" {
		alias = id
	}
	if id == "" {
		id = "auth-" + randomHex(3)
		alias = id
	}
	account := accountFromInput(in, id)
	// The alias is display-only, so it must not move the id.
	account.Alias = alias
	account.Source = "auth-file"
	account.bucket = newTokenBucket(a.currentConfig().RPM, a.currentConfig().Burst)
	if in.OrganizationCode == "" {
		// The JWT does not carry the org; resolve it before accepting the record.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		org, errOrg := a.resolveOrganization(ctx, account.Authorization)
		cancel()
		if errOrg != nil {
			return errorEnvelope("auth_error", "无法确定组织编码："+errOrg.Error()), nil
		}
		account.OrganizationCode = org
	}
	if existing := a.pool.get(account.ID); existing != nil {
		account.APIKey = existing.APIKey
		account.RefreshToken = existing.RefreshToken
		account.APIKeyExpiresAt = existing.APIKeyExpiresAt
		account.RefreshTokenExpiresAt = existing.RefreshTokenExpiresAt
		account.Device = existing.Device
		account.bucket = existing.bucket
	}
	changed := true
	if existing := a.pool.get(account.ID); existing != nil {
		changed = existing.Authorization != account.Authorization || existing.OrganizationCode != account.OrganizationCode
		// The host re-parses the record we wrote, which carries no profile data;
		// keep what the console already knows about this account.
		account.Nickname = existing.Nickname
		account.Phone = existing.Phone
		account.UserID = existing.UserID
		account.Note = existing.Note
		account.bucket = existing.bucket
		// The re-login password is ours alone: the host credential record never
		// carries it, so it has to survive the parse.
		carryPrivateFields(existing, account)
	}
	a.pool.put(account)
	a.persistAccounts()
	// No publishAccount here: the host is parsing this record right now, and a
	// save would re-enter auth.parse in an endless loop.
	if changed {
		// The profile call can take up to 20s; auth.parse must answer quickly
		// because the host walks every record through it.
		go func() {
			defer guard(a, "auth")
			a.refreshAccountInfo(account)
		}()
		a.log.info("auth", "解析到凭据："+account.label())
	}
	return okEnvelope(authParseResponse{Handled: true, Auth: a.authDataFor(account, firstNonEmpty(req.FileName, providerKey+"-"+account.ID+".json"))})
}

func (a *app) refreshAuth(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authRefreshRequest](raw)
	if err != nil {
		return nil, err
	}
	var in accountInput
	if len(req.StorageJSON) > 0 {
		_ = json.Unmarshal(req.StorageJSON, &in)
	}
	account := a.pool.get(firstNonEmpty(in.Alias, strings.TrimPrefix(req.AuthID, providerKey+"-")))
	if account == nil && in.Authorization != "" {
		account = accountFromInput(in, firstNonEmpty(in.Alias, req.AuthID))
		account.Source = "auth-file"
		cfg := a.currentConfig()
		account.bucket = newTokenBucket(cfg.RPM, cfg.Burst)
		a.pool.put(account)
		a.persistAccounts()
	}
	if account == nil {
		return errorEnvelope("auth_error", "没有可刷新的登录态"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, errKey := a.accounts.ensureAPIKey(ctx, account); errKey != nil {
		return errorEnvelope("auth_refresh_failed", asFailure(errKey).Message), nil
	}
	return okEnvelope(authRefreshResponse{
		Auth:             a.authDataFor(account, ""),
		NextRefreshAfter: time.Now().Add(90 * time.Minute),
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// resolveOrganization asks the asset API which org a JWT belongs to.
func (a *app) resolveOrganization(ctx context.Context, authorization string) (string, error) {
	up := a.upstream()
	headers := up.assetHeaders(authorization, "", "")
	delete(headers, "organization-code")
	status, header, payload, err := up.call(ctx, http.MethodGet, up.cfg.BaseURL+"/api/v1/auth/status", headers, nil)
	if err != nil {
		return "", err
	}
	data, errUnwrap := up.unwrapAsset(status, header, payload, "GET /api/v1/auth/status")
	if errUnwrap != nil {
		return "", errUnwrap
	}
	org, ok := pickOrganization(data)
	if !ok {
		return "", fmt.Errorf("auth/status 未返回组织信息：%s", truncateText(string(data), 200))
	}
	return org, nil
}

// orgEntry is one organisation as the asset API reports it. Accounts created in
// the web client come back as a flat array of these, older ones nest them under
// "organizations" with slightly different field names, so both shapes are read.
type orgEntry struct {
	OrganizationCode     string `json:"organization_code"`
	MagicOrgCode         string `json:"magic_organization_code"`
	TeamshareOrgCode     string `json:"teamshare_organization_code"`
	ThirdPlatformOrgCode string `json:"third_platform_organization_code"`
	ThirdPlatformType    string `json:"third_platform_type"`
	OrganizationName     string `json:"organization_name"`
	Current              bool   `json:"current"`
}

// code returns the most specific organisation code the entry carries.
func (e orgEntry) code() string {
	for _, candidate := range []string{e.MagicOrgCode, e.OrganizationCode, e.TeamshareOrgCode, e.ThirdPlatformOrgCode} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// pickOrganization accepts either shape and prefers the current (or first)
// organisation that actually carries a code.
func pickOrganization(data json.RawMessage) (string, bool) {
	var wrapped struct {
		Organizations []orgEntry `json:"organizations"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Organizations) > 0 {
		var fallback string
		for _, entry := range wrapped.Organizations {
			code := entry.code()
			if code == "" {
				continue
			}
			if entry.Current {
				return code, true
			}
			if fallback == "" {
				fallback = code
			}
		}
		if fallback != "" {
			return fallback, true
		}
	}
	// Flat form: [{"magic_organization_code":"...", ...}]
	var flat []orgEntry
	if err := json.Unmarshal(data, &flat); err == nil {
		for _, entry := range flat {
			if code := entry.code(); code != "" {
				return code, true
			}
		}
	}
	// Single object form.
	var single orgEntry
	if err := json.Unmarshal(data, &single); err == nil {
		if code := single.code(); code != "" {
			return code, true
		}
	}
	return "", false
}
