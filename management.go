package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

func (a *app) managementRegistration() managementRegistration {
	prefix := "/" + providerKey
	routes := []struct{ method, path, desc string }{
		{http.MethodGet, "/ui", "控制台页面"},
		{http.MethodGet, "/overview", "概览"},
		{http.MethodGet, "/accounts", "账号列表"},
		{http.MethodGet, "/accounts/export", "导出账号"},
		{http.MethodPost, "/accounts/add", "添加账号"},
		{http.MethodPost, "/accounts/delete", "删除账号"},
		{http.MethodPost, "/accounts/update", "更新账号"},
		{http.MethodPost, "/accounts/refresh", "刷新账号令牌"},
		{http.MethodGet, "/models", "模型目录"},
		{http.MethodPost, "/models/refresh", "刷新模型目录"},
		{http.MethodGet, "/requests", "请求日志"},
		{http.MethodGet, "/points", "积分与流水"},
		{http.MethodGet, "/points/summary", "全部账号积分汇总"},
		{http.MethodPost, "/points/claim-all", "全部账号领取每日积分"},
		{http.MethodPost, "/points/claim", "领取每日登录积分"},
		{http.MethodPost, "/points/task", "领取单个新人任务"},
		{http.MethodGet, "/logs", "运行日志"},
		{http.MethodGet, "/settings", "读取配置"},
		{http.MethodPost, "/settings", "保存配置"},
		{http.MethodPost, "/login/password", "手机号密码登录"},
		{http.MethodPost, "/login/captcha", "获取图形验证码"},
		{http.MethodPost, "/login/sms", "发送短信验证码"},
		{http.MethodPost, "/login/sms/verify", "短信验证码登录"},
		{http.MethodPost, "/login/token", "导入已有登录态"},
		{http.MethodPost, "/test/chat", "测试对话"},
	}
	specs := make([]managementRouteSpec, 0, len(routes))
	for _, route := range routes {
		specs = append(specs, managementRouteSpec{Method: route.method, Path: prefix + route.path, Description: route.desc})
	}
	return managementRegistration{
		Routes: specs,
		Resources: []resourceRouteSpec{
			{Path: "/ui", Menu: "Magic2API", Description: "超级麦吉反代控制台：账号池、登录、模型目录、积分与请求日志。"},
		},
	}
}

func jsonResponse(status int, body any) managementResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"ok":false,"error":"encode failed"}`)
		status = http.StatusInternalServerError
	}
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func failResponse(status int, message string) managementResponse {
	return jsonResponse(status, map[string]any{"ok": false, "error": message})
}

func okResponse(body any) managementResponse {
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "data": body})
}

// normalizeManagementPath reduces every path shape the host may hand over to the
// plugin-relative form: /ui, /magic2api/ui, /v0/management/magic2api/ui and a
// doubled prefix (which is what the console menu link used to produce) all
// collapse to /ui.
func normalizeManagementPath(raw string) string {
	path := strings.TrimSpace(raw)
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	if idx := strings.IndexByte(path, '#'); idx >= 0 {
		path = path[:idx]
	}
	path = strings.TrimPrefix(path, "/v0/management")
	path = strings.TrimPrefix(path, "/v0/resource/plugins")
	path = strings.TrimPrefix(path, "/plugins")
	for {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(path, "/"), providerKey+"/")
		if trimmed == path {
			break
		}
		path = trimmed
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return "/"
	}
	return "/" + path
}

func (a *app) handleManagement(req managementRequest) managementResponse {
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch {
	case method == http.MethodGet && (path == "/ui" || path == "/" || path == ""):
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       []byte(consoleHTML),
		}
	case method == http.MethodGet && path == "/overview":
		return okResponse(a.overview())
	case method == http.MethodGet && path == "/accounts":
		return okResponse(a.listAccounts())
	case method == http.MethodGet && path == "/accounts/export":
		return a.exportAccounts(req)
	case method == http.MethodPost && path == "/accounts/add":
		return a.managementAddAccount(req)
	case method == http.MethodPost && path == "/accounts/delete":
		return a.managementDeleteAccount(req)
	case method == http.MethodPost && path == "/accounts/update":
		return a.managementUpdateAccount(req)
	case method == http.MethodPost && path == "/accounts/refresh":
		return a.managementRefreshAccount(req)
	case method == http.MethodGet && path == "/models":
		return okResponse(a.listModels())
	case method == http.MethodPost && path == "/models/refresh":
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := a.refreshCatalog(ctx); err != nil {
			return failResponse(http.StatusBadGateway, asFailure(err).Message)
		}
		return okResponse(a.listModels())
	case method == http.MethodGet && path == "/requests":
		return okResponse(a.requests.recent(200))
	case method == http.MethodGet && path == "/points":
		return a.managementPoints(req)
	case method == http.MethodGet && path == "/points/summary":
		return a.managementPointsSummary(req)
	case method == http.MethodPost && path == "/points/claim-all":
		return a.managementClaimAll(req)
	case method == http.MethodPost && path == "/points/claim":
		return a.managementClaimGift(req)
	case method == http.MethodPost && path == "/points/task":
		return a.managementClaimTask(req)
	case method == http.MethodGet && path == "/logs":
		return okResponse(a.log.recent(300))
	case method == http.MethodGet && path == "/settings":
		return okResponse(a.settingsView())
	case method == http.MethodPost && path == "/settings":
		return a.managementSaveSettings(req)
	case method == http.MethodPost && path == "/login/password":
		return a.managementLoginPassword(req)
	case method == http.MethodPost && path == "/login/captcha":
		return a.managementLoginCaptcha(req)
	case method == http.MethodPost && path == "/login/sms":
		return a.managementLoginSMS(req)
	case method == http.MethodPost && path == "/login/sms/verify":
		return a.managementLoginSMSVerify(req)
	case method == http.MethodPost && path == "/login/token":
		return a.managementLoginToken(req)
	case method == http.MethodPost && path == "/test/chat":
		return a.managementTestChat(req)
	}
	return failResponse(http.StatusNotFound, "未知管理接口："+method+" "+path)
}

// ---- views ----

func (a *app) overview() map[string]any {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	accounts := a.pool.snapshot()
	healthy := 0
	for _, c := range accounts {
		if usable(c) && !c.cooling() {
			healthy++
		}
	}
	modelCount := 0
	for range a.registeredModels() {
		modelCount++
	}
	return map[string]any{
		"version":        pluginVersion,
		"provider":       providerKey,
		"base_url":       cfg.BaseURL,
		"account_base":   cfg.AccountBaseURL,
		"model_prefix":   cfg.ModelPrefix,
		"accounts":       len(accounts),
		"accounts_ready": healthy,
		"models":         modelCount,
		"catalog_source": snapshot.Source,
		"catalog_at":     snapshot.FetchedAt.UnixMilli(),
		"metrics":        a.metrics.snapshot(),
		"uptime_seconds": int64(time.Since(startedAt).Seconds()),
	}
}

var startedAt = time.Now()

type accountView struct {
	ID               string `json:"id"`
	Alias            string `json:"alias"`
	OrganizationCode string `json:"organization_code"`
	Priority         int    `json:"priority"`
	Disabled         bool   `json:"disabled"`
	Dead             bool   `json:"dead"`
	Source           string `json:"source"`
	Nickname         string `json:"nickname,omitempty"`
	Phone            string `json:"phone,omitempty"`
	PhoneDisplay     string `json:"phone_display,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	DeviceName       string `json:"device_name"`
	DeviceID         string `json:"device_id"`
	OS               string `json:"os"`
	APIKeyValid      bool   `json:"api_key_valid"`
	AutoRelogin      bool   `json:"auto_relogin"`
	JWTExpiresAt     int64  `json:"jwt_expires_at"`
	JWTExpired       bool   `json:"jwt_expired"`
	APIKeyExpiresAt  int64  `json:"api_key_expires_at"`
	RefreshExpiresAt int64  `json:"refresh_token_expires_at"`
	CooldownUntil    int64  `json:"cooldown_until"`
	LastSuccess      int64  `json:"last_success"`
	Failures         int    `json:"failures"`
	LastError        string `json:"last_error,omitempty"`
	Note             string `json:"note,omitempty"`
}

func (a *app) listAccounts() []accountView {
	accounts := a.pool.snapshot()
	out := make([]accountView, 0, len(accounts))
	for _, c := range accounts {
		c.mu.Lock()
		view := accountView{
			ID:               c.ID,
			Alias:            c.Alias,
			OrganizationCode: c.OrganizationCode,
			Priority:         c.Priority,
			Disabled:         c.Disabled,
			Dead:             c.Dead,
			Source:           c.Source,
			Nickname:         c.Nickname,
			Phone:            c.Phone,
			PhoneDisplay:     maskPhone(c.Phone),
			UserID:           c.UserID,
			DeviceName:       c.Device.Name,
			DeviceID:         c.Device.ID,
			OS:               strings.TrimSpace(c.Device.OS + " " + c.Device.OSVersion),
			APIKeyValid:      c.apiKeyValidLocked(0),
			AutoRelogin:      c.AutoRelogin && c.Password != "",
			JWTExpiresAt:     c.JWTExpiresAt,
			JWTExpired:       c.jwtDeadLocked(time.Now()),
			APIKeyExpiresAt:  c.APIKeyExpiresAt,
			RefreshExpiresAt: c.RefreshTokenExpiresAt,
			LastSuccess:      c.lastSuccess.UnixMilli(),
			Failures:         c.failures,
			LastError:        c.lastError,
			Note:             c.Note,
		}
		if !c.cooldownUntil.IsZero() {
			view.CooldownUntil = c.cooldownUntil.UnixMilli()
		}
		c.mu.Unlock()
		out = append(out, view)
	}
	return out
}

func (a *app) listModels() map[string]any {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	models := a.registeredModels()
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":           m.ID,
			"upstream_id":  m.Name,
			"display_name": m.DisplayName,
			"context":      m.ContextLength,
			"max_output":   m.MaxCompletionTokens,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["id"].(string) < out[j]["id"].(string)
	})
	return map[string]any{
		"prefix":     cfg.ModelPrefix,
		"source":     snapshot.Source,
		"fetched_at": snapshot.FetchedAt.UnixMilli(),
		"total":      len(out),
		"models":     out,
	}
}

func (a *app) settingsView() map[string]any {
	cfg := a.currentConfig()
	return map[string]any{
		"base_url":                cfg.BaseURL,
		"account_base_url":        cfg.AccountBaseURL,
		"model_prefix":            cfg.ModelPrefix,
		"accounts_dir":            cfg.AccountsDir,
		"data_dir":                cfg.DataDir,
		"rpm":                     cfg.RPM,
		"burst":                   cfg.Burst,
		"cooldown_seconds":        cfg.CooldownSeconds,
		"request_timeout_seconds": cfg.RequestTimeoutSeconds,
		"max_attempts":            cfg.MaxAttempts,
		"points_logging":          cfg.PointsLogging,
		"daily_points_enabled":    cfg.DailyPointsEnabled,
		"points_schedule_time":    cfg.PointsScheduleTime,
		"proxy_url":               cfg.ProxyURL,
		"debug_dump":              cfg.DebugDump,
		"import_interval_seconds": cfg.ImportInterval,
		"refresh_seconds":         cfg.RefreshSeconds,
		"version":                 pluginVersion,
	}
}

func (a *app) managementSaveSettings(req managementRequest) managementResponse {
	var patch map[string]any
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	cfg := a.currentConfig()
	if v, ok := patch["rpm"]; ok {
		cfg.RPM = toInt(v, cfg.RPM)
	}
	if v, ok := patch["burst"]; ok {
		cfg.Burst = toInt(v, cfg.Burst)
	}
	if v, ok := patch["cooldown_seconds"]; ok {
		cfg.CooldownSeconds = toInt(v, cfg.CooldownSeconds)
	}
	if v, ok := patch["request_timeout_seconds"]; ok {
		cfg.RequestTimeoutSeconds = toInt(v, cfg.RequestTimeoutSeconds)
	}
	if v, ok := patch["max_attempts"]; ok {
		cfg.MaxAttempts = toInt(v, cfg.MaxAttempts)
	}
	if v, ok := patch["points_logging"]; ok {
		cfg.PointsLogging = toBool(v, cfg.PointsLogging)
	}
	if v, ok := patch["daily_points_enabled"]; ok {
		cfg.DailyPointsEnabled = toBool(v, cfg.DailyPointsEnabled)
	}
	if v, ok := patch["points_schedule_time"]; ok {
		if s, isString := v.(string); isString && strings.TrimSpace(s) != "" {
			cfg.PointsScheduleTime = s
		}
	}
	if v, ok := patch["debug_dump"]; ok {
		cfg.DebugDump = toBool(v, cfg.DebugDump)
	}
	if v, ok := patch["proxy_url"]; ok {
		cfg.ProxyURL = strings.TrimSpace(fmt.Sprint(v))
	}
	if err := cfg.normalize(); err != nil {
		return failResponse(http.StatusBadRequest, err.Error())
	}
	a.mu.Lock()
	previous := a.cfg
	a.cfg = cfg
	if previous.ProxyURL != cfg.ProxyURL {
		old := a.up
		a.up = newUpstream(cfg)
		a.mu.Unlock()
		if old != nil {
			old.close()
		}
	} else {
		a.mu.Unlock()
	}
	for _, c := range a.pool.snapshot() {
		c.mu.Lock()
		c.bucket = newTokenBucket(cfg.RPM, cfg.Burst)
		c.mu.Unlock()
	}
	a.log.info("settings", "配置已更新")
	return okResponse(a.settingsView())
}

func toInt(v any, fallback int) int {
	switch value := v.(type) {
	case float64:
		return int(value)
	case int:
		return value
	case string:
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

func toBool(v any, fallback bool) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		return value == "true" || value == "1"
	case float64:
		return value != 0
	}
	return fallback
}

// ---- account mutations ----

func (a *app) managementAddAccount(req managementRequest) managementResponse {
	var in accountInput
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	if strings.TrimSpace(in.Authorization) == "" {
		return failResponse(http.StatusBadRequest, "authorization 不能为空")
	}
	alias := sanitizeAlias(in.Alias)
	if alias == "" {
		alias = "import-" + randomHex(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	account := accountFromInput(in, alias)
	if account.OrganizationCode == "" {
		org, err := a.resolveOrganization(ctx, account.Authorization)
		if err != nil {
			return failResponse(http.StatusBadGateway, "无法确定组织编码："+asFailure(err).Message)
		}
		account.OrganizationCode = org
	}
	account.bucket = newTokenBucket(a.currentConfig().RPM, a.currentConfig().Burst)
	// A device that came with the token may already be bound to a session, so it
	// is kept; otherwise a device id shared with another account is replaced.
	if in.Device.ID == "" || a.deviceTaken(account.Device.ID, account.ID) {
		account.Device = newDevice("import:" + alias + ":" + randomHex(4))
	}
	a.pool.put(account)
	a.persistAccounts()
	a.publishAccount(account, true)
	a.refreshAccountInfo(account)
	if _, err := a.accounts.ensureAPIKey(ctx, account); err != nil {
		a.log.warn("accounts", fmt.Sprintf("账号 %s 令牌铸造失败：%s", account.label(), asFailure(err).Message))
	}
	return okResponse(a.accountView(account))
}

func (a *app) accountView(c *credential) accountView {
	for _, view := range a.listAccounts() {
		if view.ID == c.ID {
			return view
		}
	}
	return accountView{ID: c.ID, Alias: c.Alias}
}

// accountExport is the portable credential record: everything needed to rebuild
// the account on another install. Tokens are included because this endpoint sits
// behind the host's management key.
type accountExport struct {
	Type             string     `json:"type"`
	Provider         string     `json:"provider"`
	ID               string     `json:"id"`
	Alias            string     `json:"alias"`
	Authorization    string     `json:"authorization"`
	OrganizationCode string     `json:"organization_code"`
	Device           deviceInfo `json:"device"`
	APIKey           string     `json:"api_key,omitempty"`
	RefreshToken     string     `json:"refresh_token,omitempty"`
	Password         string     `json:"password,omitempty"`
	AutoRelogin      bool       `json:"auto_relogin,omitempty"`
	APIKeyExpiresAt  int64      `json:"api_key_expires_at,omitempty"`
	RefreshExpiresAt int64      `json:"refresh_token_expires_at,omitempty"`
	Priority         int        `json:"priority"`
	Disabled         bool       `json:"disabled,omitempty"`
	Dead             bool       `json:"dead,omitempty"`
	Source           string     `json:"source,omitempty"`
	Note             string     `json:"note,omitempty"`
	Nickname         string     `json:"nickname,omitempty"`
	Phone            string     `json:"phone,omitempty"`
	UserID           string     `json:"user_id,omitempty"`
	UpdatedAt        int64      `json:"updated_at,omitempty"`
}

func exportRecord(c *credential) accountExport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return accountExport{
		Type:             providerKey,
		Provider:         providerKey,
		ID:               c.ID,
		Alias:            c.Alias,
		Authorization:    c.Authorization,
		OrganizationCode: c.OrganizationCode,
		Device:           c.Device,
		APIKey:           c.APIKey,
		RefreshToken:     c.RefreshToken,
		Password:         c.Password,
		AutoRelogin:      c.AutoRelogin,
		APIKeyExpiresAt:  c.APIKeyExpiresAt,
		RefreshExpiresAt: c.RefreshTokenExpiresAt,
		Priority:         c.Priority,
		Disabled:         c.Disabled,
		Dead:             c.Dead,
		Source:           c.Source,
		Note:             c.Note,
		Nickname:         c.Nickname,
		Phone:            c.Phone,
		UserID:           c.UserID,
		UpdatedAt:        c.UpdatedAt,
	}
}

// exportAccounts downloads one account (id) or the whole pool. A single record is
// returned bare so it can be posted straight back to /accounts/add; a pool export
// is wrapped as {"provider": ..., "accounts": [...]}.
func (a *app) exportAccounts(req managementRequest) managementResponse {
	id := strings.TrimSpace(req.Query.Get("id"))
	filename := "magic2api-accounts-" + time.Now().Format("20060102-1504") + ".json"
	var body any
	if id != "" {
		account := a.pool.get(id)
		if account == nil {
			return failResponse(http.StatusNotFound, "账号不存在："+id)
		}
		record := exportRecord(account)
		body = record
		filename = sanitizeAlias(record.Alias) + ".json"
		if filename == ".json" {
			filename = record.ID + ".json"
		}
	} else {
		accounts := a.pool.snapshot()
		records := make([]accountExport, 0, len(accounts))
		for _, account := range accounts {
			records = append(records, exportRecord(account))
		}
		body = map[string]any{
			"provider":    providerKey,
			"exported_at": time.Now().UTC().Format(time.RFC3339),
			"accounts":    records,
		}
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return failResponse(http.StatusInternalServerError, "导出失败："+err.Error())
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":        []string{"application/json; charset=utf-8"},
			"Content-Disposition": []string{`attachment; filename="` + filename + `"`},
			"Cache-Control":       []string{"no-store"},
		},
		Body: append(raw, '\n'),
	}
}

func (a *app) managementDeleteAccount(req managementRequest) managementResponse {
	id := requestAccountID(req)
	if id == "" {
		return failResponse(http.StatusBadRequest, "缺少账号 id")
	}
	if !a.pool.remove(id) {
		return failResponse(http.StatusNotFound, "账号不存在："+id)
	}
	a.persistAccounts()
	a.publishMu.Lock()
	delete(a.published, id)
	a.publishMu.Unlock()
	a.log.info("accounts", "已删除账号 "+id)
	return okResponse(map[string]any{"id": id})
}

func (a *app) managementUpdateAccount(req managementRequest) managementResponse {
	var patch struct {
		ID          string  `json:"id"`
		Priority    *int    `json:"priority"`
		Disabled    *bool   `json:"disabled"`
		Dead        *bool   `json:"dead"`
		Note        *string `json:"note"`
		Password    *string `json:"password"`
		AutoRelogin *bool   `json:"auto_relogin"`
		Alias       *string `json:"alias"`
		Phone       *string `json:"phone"`
		DeviceName  *string `json:"device_name"`
	}
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	account := a.pool.get(strings.TrimSpace(patch.ID))
	if account == nil {
		return failResponse(http.StatusNotFound, "账号不存在")
	}
	// The alias is a display name: the account id (and therefore the host's
	// credential file name) stays put, so a rename cannot orphan a credential.
	if patch.Alias != nil {
		alias := sanitizeAlias(*patch.Alias)
		if alias == "" {
			return failResponse(http.StatusBadRequest, "别名只能包含字母、数字、- _ .（会用作凭据文件名）")
		}
		for _, other := range a.pool.snapshot() {
			if other == account {
				continue
			}
			other.mu.Lock()
			taken := other.Alias == alias
			other.mu.Unlock()
			if taken {
				return failResponse(http.StatusConflict, "别名已被另一个账号使用："+alias)
			}
		}
	}
	account.mu.Lock()
	if patch.Alias != nil {
		account.Alias = sanitizeAlias(*patch.Alias)
	}
	if patch.Phone != nil {
		account.Phone = strings.TrimSpace(*patch.Phone)
	}
	if patch.DeviceName != nil && strings.TrimSpace(*patch.DeviceName) != "" {
		account.Device.Name = strings.TrimSpace(*patch.DeviceName)
	}
	if patch.Priority != nil {
		account.Priority = *patch.Priority
	}
	if patch.Disabled != nil {
		account.Disabled = *patch.Disabled
	}
	if patch.Dead != nil {
		account.Dead = *patch.Dead
		if !*patch.Dead {
			account.cooldownUntil = time.Time{}
			account.failures = 0
			account.lastError = ""
		}
	}
	if patch.Note != nil {
		account.Note = *patch.Note
	}
	if patch.Password != nil {
		account.Password = *patch.Password
	}
	if patch.AutoRelogin != nil {
		account.AutoRelogin = *patch.AutoRelogin
		// Turning it off must drop the stored secret, not just the flag.
		if !*patch.AutoRelogin {
			account.Password = ""
		}
	}
	account.UpdatedAt = time.Now().UnixMilli()
	account.mu.Unlock()
	a.persistAccounts()
	a.publishAccount(account, true)
	return okResponse(a.accountView(account))
}

func (a *app) managementRefreshAccount(req managementRequest) managementResponse {
	id := requestAccountID(req)
	account := a.pool.get(id)
	if account == nil {
		return failResponse(http.StatusNotFound, "账号不存在："+id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	a.accounts.invalidate(account)
	if _, err := a.accounts.ensureAPIKey(ctx, account); err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	a.refreshAccountInfo(account)
	return okResponse(a.accountView(account))
}

func requestAccountID(req managementRequest) string {
	if id := strings.TrimSpace(req.Query.Get("id")); id != "" {
		return id
	}
	var body struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(req.Body, &body) == nil {
		return strings.TrimSpace(body.ID)
	}
	return ""
}

// ---- points ----

// resolvePointsAccount picks the requested account, or any usable one when the
// request carries no id. An unknown id is an error: falling back silently would
// show (and claim) another account's points.
func (a *app) resolvePointsAccount(id string) (*credential, *managementResponse) {
	id = strings.TrimSpace(id)
	if id != "" {
		account := a.pool.get(id)
		if account == nil {
			resp := failResponse(http.StatusNotFound, "账号不存在："+id)
			return nil, &resp
		}
		return account, nil
	}
	account := a.pool.choose("", nil)
	if account == nil {
		resp := failResponse(http.StatusNotFound, "账号池为空")
		return nil, &resp
	}
	return account, nil
}

func (a *app) managementPoints(req managementRequest) managementResponse {
	account, resp := a.resolvePointsAccount(req.Query.Get("id"))
	if resp != nil {
		return *resp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	summary := a.summarizePoints(ctx, account, true)
	return okResponse(map[string]any{
		"account": a.accountView(account),
		"points":  summary,
	})
}

// managementPointsSummary aggregates every account so the console can show the
// pool total without opening each account. Records are cached for a minute, so
// refreshing the page does not hammer the upstream.
func (a *app) managementPointsSummary(req managementRequest) managementResponse {
	accounts := a.pool.snapshot()
	type accountPoints struct {
		ID            string  `json:"id"`
		Alias         string  `json:"alias"`
		Phone         string  `json:"phone"`
		Balance       float64 `json:"balance"`
		BalanceError  string  `json:"balance_error,omitempty"`
		TodayGift     float64 `json:"today_gift"`
		TodayTask     float64 `json:"today_task"`
		TodayConsumed float64 `json:"today_consumed"`
		PendingTasks  int     `json:"pending_tasks"`
		PendingReward float64 `json:"pending_reward"`
		GiftClaimed   bool    `json:"gift_claimed"`
		Error         string  `json:"error,omitempty"`
	}

	rows := make([]accountPoints, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, account := range accounts {
		wg.Add(1)
		go func(index int, account *credential) {
			defer wg.Done()
			defer guard(a, "points-summary")
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			summary := a.summarizePoints(ctx, account, true)

			row := accountPoints{
				ID:            account.ID,
				Alias:         account.Alias,
				Phone:         maskPhone(account.Phone),
				Balance:       summary.Balance.TotalPoints.Float64(),
				BalanceError:  summary.BalanceError,
				TodayGift:     summary.Today.GiftPoints,
				TodayTask:     summary.Today.TaskPoints,
				TodayConsumed: -summary.Today.ConsumedPoints,
				GiftClaimed:   summary.Today.LastClaimedDate != "",
			}
			if row.TodayConsumed < 0 {
				row.TodayConsumed = 0
			}
			for _, task := range summary.Tasks {
				if task.Status == "PENDING" && task.RewardStatus == "PENDING" && task.Code != "first_ai_work" {
					row.PendingTasks++
					row.PendingReward += task.Points.Float64()
				}
			}
			if summary.RecordsError != "" {
				row.Error = summary.RecordsError
			}
			rows[index] = row
		}(i, account)
	}
	wg.Wait()

	total := accountPoints{Alias: "合计"}
	for _, row := range rows {
		total.Balance += row.Balance
		total.TodayGift += row.TodayGift
		total.TodayTask += row.TodayTask
		total.TodayConsumed += row.TodayConsumed
		total.PendingTasks += row.PendingTasks
		total.PendingReward += row.PendingReward
		if row.GiftClaimed {
			total.GiftClaimed = true
		}
	}
	claimed := 0
	for _, row := range rows {
		if row.GiftClaimed {
			claimed++
		}
	}
	return okResponse(map[string]any{
		"accounts": rows,
		"totals": map[string]any{
			"balance":            total.Balance,
			"today_gift":         total.TodayGift,
			"today_task":         total.TodayTask,
			"today_consumed":     total.TodayConsumed,
			"pending_tasks":      total.PendingTasks,
			"pending_reward":     total.PendingReward,
			"accounts":           len(rows),
			"gift_claimed":       claimed,
			"gift_claimed_count": claimed,
		},
		"schedule":   a.pointsScheduleView(a.currentConfig()),
		"fetched_at": time.Now().UnixMilli(),
	})
}

// managementClaimAll runs the daily claim for every usable account, the same
// routine the scheduler uses.
func (a *app) managementClaimAll(req managementRequest) managementResponse {
	today := time.Now().In(serverZone).Format("2006-01-02")
	claimed, skipped, failed := 0, 0, 0
	messages := make([]string, 0, 4)
	for _, account := range a.pool.snapshot() {
		if !usable(account) {
			skipped++
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		ok := a.claimDaily(ctx, account, today)
		cancel()
		if ok {
			claimed++
		} else {
			failed++
			if len(messages) < 3 {
				messages = append(messages, account.label())
			}
		}
	}
	return okResponse(map[string]any{
		"claimed":  claimed,
		"skipped":  skipped,
		"failed":   failed,
		"problems": messages,
		"message":  fmt.Sprintf("领取完成：成功 %d，跳过 %d，失败 %d", claimed, skipped, failed),
	})
}

func (a *app) managementClaimGift(req managementRequest) managementResponse {
	account, resp := a.resolvePointsAccount(requestAccountID(req))
	if resp != nil {
		return *resp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	gift, err := a.claimLoginGift(ctx, account)
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	a.invalidatePoints(account.ID)
	return okResponse(gift)
}

func (a *app) managementClaimTask(req managementRequest) managementResponse {
	var body struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	account, resp := a.resolvePointsAccount(body.ID)
	if resp != nil {
		return *resp
	}
	if strings.TrimSpace(body.Code) == "" {
		return failResponse(http.StatusBadRequest, "缺少任务 code")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	points, err := a.completeOnboardingTask(ctx, account, body.Code)
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	a.invalidatePoints(account.ID)
	return okResponse(map[string]any{"code": body.Code, "points": points})
}

// ---- login ----

func (a *app) managementLoginPassword(req managementRequest) managementResponse {
	var body struct {
		Phone     string `json:"phone"`
		Password  string `json:"password"`
		StateCode string `json:"state_code"`
		Alias     string `json:"alias"`
		State     string `json:"state"`
		// Remember keeps the password so the 30-day login token can be renewed
		// without a human. Opt-in: without it nothing is stored.
		Remember bool `json:"remember"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	if strings.TrimSpace(body.Phone) == "" || strings.TrimSpace(body.Password) == "" {
		return failResponse(http.StatusBadRequest, "手机号与密码都必须填写")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	device := newDevice("login:" + body.Phone)
	token, err := a.callLogin(ctx, loginPayload{
		Phone:     strings.TrimSpace(body.Phone),
		StateCode: body.StateCode,
		Password:  body.Password,
		Type:      "phone_password",
	}, device)
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	alias := sanitizeAlias(body.Alias)
	if alias == "" {
		alias = "phone-" + tailDigits(body.Phone, 4)
	}
	account, err := a.adoptLogin(ctx, token, alias, strings.TrimSpace(body.Phone))
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	if body.Remember {
		account.mu.Lock()
		account.Password = body.Password
		account.AutoRelogin = true
		account.mu.Unlock()
		a.persistAccounts()
		a.log.info("login", "账号 "+account.label()+" 已开启到期自动重登（保存了密码）")
	}
	a.completeLoginSession(strings.TrimSpace(body.State), account)
	return okResponse(a.accountView(account))
}

func (a *app) managementLoginCaptcha(req managementRequest) managementResponse {
	var body struct {
		Phone     string `json:"phone"`
		StateCode string `json:"state_code"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	if strings.TrimSpace(body.Phone) == "" {
		return failResponse(http.StatusBadRequest, "请先填写手机号")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	challenge, err := a.generateCaptcha(ctx, strings.TrimSpace(body.Phone), body.StateCode)
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	return okResponse(challenge)
}

func (a *app) managementLoginSMS(req managementRequest) managementResponse {
	var body struct {
		Phone        string `json:"phone"`
		StateCode    string `json:"state_code"`
		CaptchaToken string `json:"captcha_token"`
		Angle        int    `json:"angle"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	verified, err := a.verifyCaptcha(ctx, body.CaptchaToken, body.Angle, strings.TrimSpace(body.Phone), body.StateCode)
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	if !verified.Verified || verified.BusinessToken == "" {
		return failResponse(http.StatusBadRequest, "图形验证码未通过")
	}
	if err := a.sendSMSCode(ctx, strings.TrimSpace(body.Phone), body.StateCode, verified.BusinessToken); err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	return okResponse(map[string]any{"sent": true})
}

func (a *app) managementLoginSMSVerify(req managementRequest) managementResponse {
	var body struct {
		Phone     string `json:"phone"`
		StateCode string `json:"state_code"`
		Code      string `json:"code"`
		Alias     string `json:"alias"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	if strings.TrimSpace(body.Code) == "" {
		return failResponse(http.StatusBadRequest, "请填写短信验证码")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	device := newDevice("login:" + body.Phone)
	token, err := a.callLogin(ctx, loginPayload{
		Phone:        strings.TrimSpace(body.Phone),
		StateCode:    body.StateCode,
		Code:         strings.TrimSpace(body.Code),
		AutoRegister: true,
		Type:         "phone_captcha",
	}, device)
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	alias := sanitizeAlias(body.Alias)
	if alias == "" {
		alias = "phone-" + tailDigits(body.Phone, 4)
	}
	account, err := a.adoptLogin(ctx, token, alias, strings.TrimSpace(body.Phone))
	if err != nil {
		return failResponse(http.StatusBadGateway, asFailure(err).Message)
	}
	a.completeLoginSession(strings.TrimSpace(body.State), account)
	return okResponse(a.accountView(account))
}

func (a *app) managementLoginToken(req managementRequest) managementResponse {
	var body struct {
		Authorization    string `json:"authorization"`
		OrganizationCode string `json:"organization_code"`
		Alias            string `json:"alias"`
		State            string `json:"state"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	if strings.TrimSpace(body.Authorization) == "" {
		return failResponse(http.StatusBadRequest, "authorization 不能为空")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	org := strings.TrimSpace(body.OrganizationCode)
	if org == "" {
		resolved, err := a.resolveOrganization(ctx, body.Authorization)
		if err != nil {
			return failResponse(http.StatusBadGateway, "无法确定组织编码："+asFailure(err).Message)
		}
		org = resolved
	}
	alias := sanitizeAlias(body.Alias)
	if alias == "" {
		alias = "token-" + randomHex(3)
	}
	account := &credential{
		ID:               accountIDForAlias(alias),
		Alias:            alias,
		Authorization:    strings.TrimSpace(body.Authorization),
		OrganizationCode: org,
		Device:           newDevice("login:" + alias + ":" + randomHex(4)),
		Priority:         5,
		Source:           "token",
		UpdatedAt:        time.Now().UnixMilli(),
	}
	if existing := a.pool.get(account.ID); existing != nil {
		account.Device = existing.Device
	}
	account.bucket = newTokenBucket(a.currentConfig().RPM, a.currentConfig().Burst)
	a.pool.put(account)
	a.persistAccounts()
	a.publishAccount(account, true)
	a.refreshAccountInfo(account)
	if _, err := a.accounts.ensureAPIKey(ctx, account); err != nil {
		a.log.warn("accounts", fmt.Sprintf("账号 %s 令牌铸造失败：%s", account.label(), asFailure(err).Message))
	}
	a.completeLoginSession(strings.TrimSpace(body.State), account)
	return okResponse(a.accountView(account))
}

func tailDigits(value string, n int) string {
	digits := make([]rune, 0, len(value))
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) <= n {
		return string(digits)
	}
	return string(digits[len(digits)-n:])
}

// ---- test chat ----

func (a *app) managementTestChat(req managementRequest) managementResponse {
	var body struct {
		Model    string `json:"model"`
		Prompt   string `json:"prompt"`
		Stream   bool   `json:"stream"`
		MaxToken int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	cfg := a.currentConfig()
	model := strings.TrimSpace(body.Model)
	if model == "" {
		models := a.registeredModels()
		if len(models) == 0 {
			return failResponse(http.StatusServiceUnavailable, "模型目录为空")
		}
		model = models[0].ID
	}
	upstreamModel, ok := cfg.upstreamModel(model)
	if !ok {
		return failResponse(http.StatusBadRequest, "模型不属于 magic2api："+model)
	}
	if body.Prompt == "" {
		body.Prompt = "用一句话介绍你自己"
	}
	if body.MaxToken <= 0 {
		body.MaxToken = 256
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      upstreamModel,
		"messages":   []map[string]string{{"role": "user", "content": body.Prompt}},
		"max_tokens": body.MaxToken,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	started := time.Now()
	result, account, f := a.dispatch(ctx, cfg, payload, false, "")
	if f != nil {
		return failResponse(f.Status, f.Message)
	}
	view := map[string]any{
		"model":    model,
		"elapsed":  time.Since(started).Milliseconds(),
		"response": json.RawMessage(result.body),
	}
	if account != nil {
		view["account"] = account.label()
	}
	return okResponse(view)
}

// maskPhone keeps the console readable: a real number is masked, an already
// masked one (from upstream) is passed through unchanged.
func maskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if !isRealPhone(phone) {
		return phone
	}
	if len(phone) <= 7 {
		return phone[:1] + "****" + phone[len(phone)-2:]
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}
