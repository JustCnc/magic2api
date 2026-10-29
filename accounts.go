package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// credential is one account: the user JWT plus the model-gateway pair minted
// from it. The JWT lives 30 days and cannot be refreshed, the api key 2 hours.
type credential struct {
	ID               string     `json:"id"`
	Alias            string     `json:"alias"`
	Authorization    string     `json:"authorization"`
	OrganizationCode string     `json:"organization_code"`
	Device           deviceInfo `json:"device"`

	// JWTExpiresAt is read from the JWT exp claim at load/login time. It never
	// changes while the account lives: the login token cannot be refreshed, only
	// re-issued by signing in again.
	JWTExpiresAt int64 `json:"jwt_expires_at,omitempty"`
	// Password is opt-in: only stored when the user asked for automatic
	// re-login, because the 30-day login token can only be renewed by signing in
	// again. Plain text, like the tokens next to it.
	Password              string `json:"password,omitempty"`
	AutoRelogin           bool   `json:"auto_relogin,omitempty"`
	APIKey                string `json:"api_key,omitempty"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	APIKeyExpiresAt       int64  `json:"api_key_expires_at,omitempty"`
	RefreshTokenExpiresAt int64  `json:"refresh_token_expires_at,omitempty"`

	Priority int    `json:"priority"`
	Disabled bool   `json:"disabled,omitempty"`
	Dead     bool   `json:"dead,omitempty"`
	Source   string `json:"source,omitempty"`
	Note     string `json:"note,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Phone    string `json:"phone,omitempty"`
	UserID   string `json:"user_id,omitempty"`

	UpdatedAt int64 `json:"updated_at,omitempty"`

	// runtime only
	mu sync.Mutex `json:"-"`
	// claimMu serialises daily-points claims without blocking console reads.
	claimMu       sync.Mutex `json:"-"`
	reloginMu     sync.Mutex `json:"-"`
	reloginAt     time.Time  `json:"-"`
	reloginFails  int        `json:"-"`
	cooldownUntil time.Time  `json:"-"`
	lastSuccess   time.Time  `json:"-"`
	failures      int        `json:"-"`
	lastError     string     `json:"-"`
	bucket        *tokenBucket
}

func (c *credential) apiKeyValid(margin time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiKeyValidLocked(margin)
}

// apiKeyValidLocked is the lock-free form for callers already holding c.mu.
func (c *credential) apiKeyValidLocked(margin time.Duration) bool {
	if c.APIKey == "" || c.APIKeyExpiresAt == 0 {
		return false
	}
	return time.Now().Add(margin).Before(time.UnixMilli(c.APIKeyExpiresAt))
}

func (c *credential) cooling() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.cooldownUntil)
}

// exportSnapshot copies the persisted fields under the lock so json.Marshal never
// reads a string header while another goroutine rewrites it (which would be a
// torn read inside the host process).
func (c *credential) exportSnapshot() credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	return credential{
		ID:                    c.ID,
		Alias:                 c.Alias,
		Authorization:         c.Authorization,
		OrganizationCode:      c.OrganizationCode,
		Device:                c.Device,
		APIKey:                c.APIKey,
		RefreshToken:          c.RefreshToken,
		APIKeyExpiresAt:       c.APIKeyExpiresAt,
		RefreshTokenExpiresAt: c.RefreshTokenExpiresAt,
		Priority:              c.Priority,
		Disabled:              c.Disabled,
		Dead:                  c.Dead,
		Source:                c.Source,
		Note:                  c.Note,
		Nickname:              c.Nickname,
		Phone:                 c.Phone,
		UserID:                c.UserID,
		UpdatedAt:             c.UpdatedAt,
	}
}

func (c *credential) label() string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.ID
}

// accountInput is the JSON shape accepted from the console, an accounts_dir file
// or a CPA auth record. Field names stay close to what the plugin exports.
type accountInput struct {
	Type             string     `json:"type,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	Alias            string     `json:"alias,omitempty"`
	Authorization    string     `json:"authorization,omitempty"`
	OrganizationCode string     `json:"organization_code,omitempty"`
	Device           deviceInfo `json:"device,omitempty"`
	DeviceID         string     `json:"device_id,omitempty"`
	DeviceName       string     `json:"device_name,omitempty"`
	Priority         int        `json:"priority,omitempty"`
	Disabled         bool       `json:"disabled,omitempty"`
	Dead             bool       `json:"dead,omitempty"`
	Note             string     `json:"note,omitempty"`
	APIKey           string     `json:"api_key,omitempty"`
	RefreshToken     string     `json:"refresh_token,omitempty"`
	Password         string     `json:"password,omitempty"`
	AutoRelogin      bool       `json:"auto_relogin,omitempty"`
}

// credentialFromConfig builds the primary account, if configured.
func credentialFromConfig(cfg Config) *credential {
	if cfg.Authorization == "" || cfg.OrganizationCode == "" {
		return nil
	}
	device := newDevice("config:" + cfg.CredentialAlias)
	if cfg.DeviceID != "" {
		device.ID = cfg.DeviceID
	}
	if cfg.DeviceName != "" {
		device.Name = cfg.DeviceName
	}
	return &credential{
		ID:               accountIDForAlias(cfg.CredentialAlias),
		Alias:            cfg.CredentialAlias,
		Authorization:    cfg.Authorization,
		OrganizationCode: cfg.OrganizationCode,
		Device:           device,
		Priority:         cfg.CredentialPriority,
		Source:           "config",
		UpdatedAt:        time.Now().UnixMilli(),
	}
}

// accountFromInput normalises an imported JSON record.
func accountFromInput(in accountInput, alias string) *credential {
	device := in.Device
	if device.ID == "" {
		device = newDevice("import:" + alias)
	}
	if in.DeviceID != "" {
		device.ID = in.DeviceID
	}
	if in.DeviceName != "" {
		device.Name = in.DeviceName
	}
	prio := in.Priority
	if prio <= 0 {
		prio = 5
	}
	return &credential{
		ID:               accountIDForAlias(alias),
		Alias:            alias,
		Authorization:    strings.TrimSpace(in.Authorization),
		OrganizationCode: strings.TrimSpace(in.OrganizationCode),
		Device:           device,
		Priority:         prio,
		Disabled:         in.Disabled,
		Dead:             in.Dead,
		Note:             in.Note,
		APIKey:           strings.TrimSpace(in.APIKey),
		RefreshToken:     strings.TrimSpace(in.RefreshToken),
		Password:         in.Password,
		AutoRelogin:      in.AutoRelogin && in.Password != "",
		Source:           "import",
		UpdatedAt:        time.Now().UnixMilli(),
	}
}

func accountIDForAlias(alias string) string {
	alias = sanitizeAlias(alias)
	if alias == "" {
		alias = "account"
	}
	return alias
}

// tokenBucket is a per-account rate limiter. Exceeding it answers a local 429
// so the host switches credentials instead of queueing behind one account.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rpm, burst int) *tokenBucket {
	if rpm <= 0 {
		rpm = 30
	}
	if burst <= 0 {
		burst = 20
	}
	return &tokenBucket{
		rate:   float64(rpm) / 60.0,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// pool is the account store.
type pool struct {
	mu       sync.RWMutex
	accounts map[string]*credential
	order    []string
}

func newPool() *pool {
	return &pool{accounts: make(map[string]*credential)}
}

func (p *pool) put(c *credential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c != nil {
		// Derive the JWT deadline from the token itself so a re-login that swaps
		// the authorization always picks up the new exp.
		c.mu.Lock()
		if exp := jwtExpiresAtMillis(c.Authorization); exp > 0 {
			c.JWTExpiresAt = exp
		}
		c.mu.Unlock()
	}
	if _, exists := p.accounts[c.ID]; !exists {
		p.order = append(p.order, c.ID)
	}
	p.accounts[c.ID] = c
}

func (p *pool) get(id string) *credential {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accounts[id]
}

func (p *pool) remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.accounts[id]; !ok {
		return false
	}
	delete(p.accounts, id)
	for i, existing := range p.order {
		if existing == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	return true
}

// snapshot returns accounts ordered by descending priority then insertion.
func (p *pool) snapshot() []*credential {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*credential, 0, len(p.order))
	for _, id := range p.order {
		if c, ok := p.accounts[id]; ok {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// choose picks the best usable account, skipping ids already tried by this
// request. Preference: not cooling, higher priority, most recent success.
func (p *pool) choose(preferred string, tried map[string]bool) *credential {
	candidates := p.snapshot()
	if preferred != "" {
		for _, c := range candidates {
			// A cooling account must not win on stickiness alone: it would burn a
			// whole attempt on a request that is expected to fail.
			if c.ID == preferred && usable(c) && !c.cooling() && !tried[c.ID] {
				return c
			}
		}
	}
	var best *credential
	for _, c := range candidates {
		if !usable(c) || tried[c.ID] {
			continue
		}
		if best == nil {
			best = c
			continue
		}
		bestCooling, candCooling := best.cooling(), c.cooling()
		if bestCooling != candCooling {
			if bestCooling {
				best = c
			}
			continue
		}
		c.mu.Lock()
		priority, updated, lastSuccess := c.Priority, c.UpdatedAt, c.lastSuccess
		c.mu.Unlock()
		best.mu.Lock()
		bestPriority, bestUpdated, bestSuccess := best.Priority, best.UpdatedAt, best.lastSuccess
		best.mu.Unlock()
		if priority != bestPriority {
			if priority > bestPriority {
				best = c
			}
			continue
		}
		if lastSuccess.After(bestSuccess) {
			best = c
			continue
		}
		if lastSuccess.Equal(bestSuccess) && updated > bestUpdated {
			best = c
		}
	}
	return best
}

// usable reports whether an account may serve traffic.
func usable(c *credential) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return usableLocked(c)
}

// usableLocked is the lock-free form for callers already holding c.mu. An
// account whose login token expired can still serve from its cached api key
// until that key's own TTL runs out; after that nobody can mint a new one.
func usableLocked(c *credential) bool {
	if c == nil || c.Dead || c.Disabled || c.Authorization == "" || c.OrganizationCode == "" {
		return false
	}
	if c.jwtDeadLocked(time.Now()) && !c.apiKeyValidLocked(0) {
		return false
	}
	return true
}

// accountManager owns token rotation and health state.
type accountManager struct {
	app *app

	mu     sync.Mutex
	flight map[string]chan struct{}
}

func newAccountManager(a *app) *accountManager {
	return &accountManager{app: a, flight: make(map[string]chan struct{})}
}

// ensureAPIKey returns a live api key, minting or refreshing as needed. Calls
// for the same account are serialised so a burst triggers one refresh.
func (m *accountManager) ensureAPIKey(ctx context.Context, c *credential) (string, error) {
	if c == nil {
		return "", &failure{Status: http.StatusUnauthorized, Message: "没有可用账号"}
	}
	if c.apiKeyValid(5 * time.Minute) {
		return c.APIKey, nil
	}
	if exp, dead := c.jwtDeadline(); dead {
		return "", &failure{
			Status:  http.StatusUnauthorized,
			Auth:    true,
			Message: fmt.Sprintf("账号 %s 的登录令牌已于 %s 过期，请重新登录", c.label(), exp.Format("2006-01-02 15:04")),
		}
	}
	m.mu.Lock()
	if ch, busy := m.flight[c.ID]; busy {
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			// A cancelled caller is not a rejected credential: do not cool down.
			return "", &failure{Status: 499, Message: "请求已取消"}
		}
		if c.apiKeyValid(time.Minute) {
			return c.APIKey, nil
		}
		// The in-flight refresh failed. That is not proof the credential is bad,
		// so leave the account health alone.
		c.mu.Lock()
		lastErr := c.lastError
		c.mu.Unlock()
		return "", &failure{Status: http.StatusBadGateway, Message: firstNonEmpty(lastErr, "令牌刷新失败"), Retryable: true}
	}
	ch := make(chan struct{})
	m.flight[c.ID] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.flight, c.ID)
		m.mu.Unlock()
		close(ch)
	}()

	up := m.app.upstream()

	// Prefer a refresh: it keeps the same refresh chain and costs one call.
	if c.RefreshToken != "" && time.Now().UnixMilli() < c.RefreshTokenExpiresAt {
		tok, err := up.refreshGatewayToken(ctx, c.Authorization, c.OrganizationCode, c.RefreshToken)
		if err == nil {
			m.applyToken(c, tok)
			m.app.log.info("token", fmt.Sprintf("账号 %s 刷新模型网关令牌成功", c.label()))
			return c.APIKey, nil
		}
		if f := asFailure(err); f.Auth || !f.Retryable {
			// A rejected refresh token must not block a fresh mint.
			m.app.log.warn("token", fmt.Sprintf("账号 %s 刷新失败（%s），改为重新铸造", c.label(), f.Message))
		}
	}
	tok, err := up.mintGatewayToken(ctx, c.Authorization, c.OrganizationCode)
	if err != nil {
		return "", err
	}
	m.applyToken(c, tok)
	m.app.log.info("token", fmt.Sprintf("账号 %s 铸造模型网关令牌成功", c.label()))
	return c.APIKey, nil
}

func (m *accountManager) applyToken(c *credential, tok gatewayToken) {
	c.mu.Lock()
	c.APIKey = tok.APIKey
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	c.APIKeyExpiresAt = time.Now().Add(ttlFrom(tok.APIKeyExpiresAt, 2*time.Hour)).UnixMilli()
	c.RefreshTokenExpiresAt = time.Now().Add(ttlFrom(tok.RefreshTokenExpiresAt, 7*24*time.Hour)).UnixMilli()
	c.UpdatedAt = time.Now().UnixMilli()
	c.mu.Unlock()
	m.app.persistAccounts()
}

// invalidate drops the cached api key after an upstream 401/403.
func (m *accountManager) invalidate(c *credential) {
	c.mu.Lock()
	c.APIKey = ""
	c.APIKeyExpiresAt = 0
	c.mu.Unlock()
}

func (m *accountManager) markSuccess(c *credential) {
	c.mu.Lock()
	c.lastSuccess = time.Now()
	c.failures = 0
	c.lastError = ""
	c.mu.Unlock()
}

func (m *accountManager) markFailure(c *credential, f *failure) {
	if c == nil || f == nil {
		return
	}
	cfg := m.app.currentConfig()
	c.mu.Lock()
	c.failures++
	c.lastError = f.Message
	switch {
	case f.Dead:
		c.Dead = true
	case f.Auth:
		c.cooldownUntil = time.Now().Add(time.Duration(cfg.CooldownSeconds) * time.Second)
	case f.RateLimit:
		cooldown := cfg.CooldownSeconds
		if cooldown < 30 {
			cooldown = 30
		}
		c.cooldownUntil = time.Now().Add(time.Duration(cooldown) * time.Second)
	case f.Retryable:
		c.cooldownUntil = time.Now().Add(time.Duration(cfg.CooldownSeconds) * time.Second)
	}
	c.mu.Unlock()
}

// ---- persistence ----

type accountsFile struct {
	Version  int           `json:"version"`
	Accounts []*credential `json:"accounts"`
}

func (a *app) loadPool() {
	store := a.stateStore()
	if store == nil {
		return
	}
	var file accountsFile
	if ok, err := store.readJSON("accounts.json", &file); err == nil && ok {
		for _, c := range file.Accounts {
			if c == nil || c.Authorization == "" {
				continue
			}
			c.bucket = newTokenBucket(a.currentConfig().RPM, a.currentConfig().Burst)
			if c.Device.ID == "" {
				c.Device = newDevice("import:" + c.Alias)
			}
			a.pool.put(c)
		}
	}
	cfg := a.currentConfig()
	if primary := credentialFromConfig(cfg); primary != nil {
		if existing := a.pool.get(primary.ID); existing != nil {
			// A config edit wins over the persisted copy.
			primary.APIKey = existing.APIKey
			primary.RefreshToken = existing.RefreshToken
			primary.APIKeyExpiresAt = existing.APIKeyExpiresAt
			primary.RefreshTokenExpiresAt = existing.RefreshTokenExpiresAt
			primary.Device = existing.Device
		}
		primary.bucket = newTokenBucket(cfg.RPM, cfg.Burst)
		a.pool.put(primary)
	}
	a.scanAccountsDir(cfg)
}

// deviceTaken reports whether another account already uses this device id.
func (a *app) deviceTaken(deviceID, selfID string) bool {
	if deviceID == "" {
		return false
	}
	for _, other := range a.pool.snapshot() {
		if other.ID != selfID && other.Device.ID == deviceID {
			return true
		}
	}
	return false
}

func (a *app) persistAccounts() {
	store := a.stateStore()
	if store == nil {
		return
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	accounts := a.pool.snapshot()
	file := accountsFile{Version: 1, Accounts: accounts}
	if err := store.writeJSON("accounts.json", file); err != nil {
		a.log.warn("state", "写入 accounts.json 失败："+err.Error())
	}
}

// scanAccountsDir imports every *.json in accounts_dir. The file name is the
// alias; a file that disappears removes the account.
func (a *app) scanAccountsDir(cfg Config) {
	dir := strings.TrimSpace(cfg.AccountsDir)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		alias := sanitizeAlias(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		if alias == "" {
			continue
		}
		id := accountIDForAlias(alias)
		seen[id] = true
		raw, errRead := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errRead != nil {
			continue
		}
		var in accountInput
		if errUnmarshal := json.Unmarshal(raw, &in); errUnmarshal != nil || in.Authorization == "" {
			continue
		}
		account := accountFromInput(in, alias)
		account.Source = "accounts-dir"
		account.UpdatedAt = time.Now().UnixMilli()
		if existing := a.pool.get(id); existing != nil {
			if existing.Authorization == account.Authorization {
				// Unchanged: keep the minted tokens.
				continue
			}
			account.APIKey = existing.APIKey
			account.RefreshToken = existing.RefreshToken
			account.APIKeyExpiresAt = existing.APIKeyExpiresAt
			account.RefreshTokenExpiresAt = existing.RefreshTokenExpiresAt
			carryPrivateFields(existing, account)
		}
		account.bucket = newTokenBucket(cfg.RPM, cfg.Burst)
		a.pool.put(account)
	}
	for _, c := range a.pool.snapshot() {
		if c.Source == "accounts-dir" && !seen[c.ID] {
			a.pool.remove(c.ID)
			a.log.info("accounts", "账号文件已删除，移出池子："+c.label())
			// Without this the account comes back on the next start, then gets
			// removed again on the following scan.
			a.persistAccounts()
		}
	}
}

// carryPrivateFields copies the state that only lives in the plugin (never in a
// host credential record) onto a credential rebuilt from such a record.
func carryPrivateFields(existing, account *credential) {
	existing.mu.Lock()
	password, auto := existing.Password, existing.AutoRelogin
	if existing.Phone != "" && !isRealPhone(account.Phone) {
		account.Phone = existing.Phone
	}
	existing.mu.Unlock()
	if password != "" && account.Password == "" {
		account.Password = password
		account.AutoRelogin = auto
	}
}

// unixMilliOrZero keeps Go's zero time out of JSON: 0001-01-01 would render as a
// bogus date in the console instead of "—".
func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
