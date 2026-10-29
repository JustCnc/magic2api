package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (a *app) managementRegistration() managementRegistration {
	prefix := "/" + providerKey
	routes := []struct{ method, path, desc string }{
		{http.MethodGet, "/ui", "控制台页面"},
		{http.MethodGet, "/overview", "概览"},
		{http.MethodGet, "/accounts", "账号列表"},
		{http.MethodPost, "/accounts/add", "添加账号"},
		{http.MethodPost, "/accounts/delete", "删除账号"},
		{http.MethodPost, "/accounts/update", "更新账号"},
		{http.MethodPost, "/accounts/refresh", "刷新账号令牌"},
		{http.MethodGet, "/models", "模型目录"},
		{http.MethodPost, "/models/refresh", "刷新模型目录"},
		{http.MethodGet, "/requests", "请求日志"},
		{http.MethodGet, "/points", "积分与流水"},
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
			{Path: prefix + "/ui", Menu: "Magic2API", Description: "超级麦吉反代控制台：账号池、登录、模型目录、积分与请求日志。"},
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

func (a *app) handleManagement(req managementRequest) managementResponse {
	path := strings.TrimSpace(req.Path)
	if idx := strings.Index(path, "/"+providerKey+"/"); idx >= 0 {
		path = path[idx+len(providerKey)+2:]
	}
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	path = "/" + strings.TrimPrefix(path, "/")
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
	UserID           string `json:"user_id,omitempty"`
	DeviceName       string `json:"device_name"`
	DeviceID         string `json:"device_id"`
	OS               string `json:"os"`
	APIKeyValid      bool   `json:"api_key_valid"`
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
			UserID:           c.UserID,
			DeviceName:       c.Device.Name,
			DeviceID:         c.Device.ID,
			OS:               strings.TrimSpace(c.Device.OS + " " + c.Device.OSVersion),
			APIKeyValid:      c.apiKeyValidLocked(0),
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
		ID       string  `json:"id"`
		Priority *int    `json:"priority"`
		Disabled *bool   `json:"disabled"`
		Dead     *bool   `json:"dead"`
		Note     *string `json:"note"`
	}
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return failResponse(http.StatusBadRequest, "请求体不是 JSON："+err.Error())
	}
	account := a.pool.get(strings.TrimSpace(patch.ID))
	if account == nil {
		return failResponse(http.StatusNotFound, "账号不存在")
	}
	account.mu.Lock()
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
