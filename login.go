package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// loginSession tracks one console/CPA login attempt.
type loginSession struct {
	State   string
	Expires time.Time
	Auth    *credential
	Err     string
}

func (a *app) newLoginSession() *loginSession {
	session := &loginSession{State: randomHex(16), Expires: time.Now().Add(15 * time.Minute)}
	a.loginMu.Lock()
	if a.logins == nil {
		a.logins = make(map[string]*loginSession)
	}
	now := time.Now()
	for state, existing := range a.logins {
		if now.After(existing.Expires) {
			delete(a.logins, state)
		}
	}
	a.logins[session.State] = session
	a.loginMu.Unlock()
	return session
}

// completeLoginSession hands a finished login to the host's poll call. The
// console carries the state it was opened with, so a browser login can close a
// CPA-initiated session instead of leaving it pending forever.
func (a *app) completeLoginSession(state string, account *credential) {
	if state == "" || account == nil {
		return
	}
	a.loginMu.Lock()
	session := a.logins[state]
	if session != nil {
		session.Auth = account
	}
	a.loginMu.Unlock()
}

func (a *app) loginSessionByState(state string) *loginSession {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	return a.logins[state]
}

// startLogin answers CPA's auth.login.start. The Magic client has no OAuth
// authorization link, so the "URL" points at this plugin's own console tab that
// drives the official login endpoints (phone+password, or SMS with the official
// rotate captcha).
func (a *app) startLogin(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authLoginStartRequest](raw)
	if err != nil {
		return nil, err
	}
	session := a.newLoginSession()
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	url := ""
	if base != "" {
		url = fmt.Sprintf("%s/v0/management/%s/ui?state=%s#login", base, providerKey, session.State)
	}
	a.log.info("login", "已创建登录会话 "+session.State)
	return okEnvelope(authLoginStartResponse{
		Provider:  providerKey,
		URL:       url,
		State:     session.State,
		ExpiresAt: session.Expires,
	})
}

func (a *app) pollLogin(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authLoginPollRequest](raw)
	if err != nil {
		return nil, err
	}
	session := a.loginSessionByState(req.State)
	if session == nil {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: "登录会话不存在或已过期，请在控制台重新发起"})
	}
	if session.Auth != nil {
		a.loginMu.Lock()
		delete(a.logins, req.State)
		a.loginMu.Unlock()
		return okEnvelope(authLoginPollResponse{
			Status:  "success",
			Message: "登录成功",
			Auth:    a.authDataFor(session.Auth, providerKey+"-"+session.Auth.ID+".json"),
		})
	}
	if session.Err != "" {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: session.Err})
	}
	if time.Now().After(session.Expires) {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: "登录会话已过期"})
	}
	return okEnvelope(authLoginPollResponse{Status: "pending", Message: "等待浏览器完成登录"})
}

// ---- official login endpoints (keewood host) ----

type loginResponse struct {
	AccessToken string `json:"access_token"`
}

// loginPayload is the body POST /v4/user/login expects. The client always sends
// redirect and device; auto_register only matters for SMS sign-up.
type loginPayload struct {
	Phone        string     `json:"phone"`
	StateCode    string     `json:"state_code"`
	Password     string     `json:"password,omitempty"`
	Code         string     `json:"code,omitempty"`
	AutoRegister bool       `json:"auto_register,omitempty"`
	Type         string     `json:"type"`
	Redirect     string     `json:"redirect"`
	Device       deviceInfo `json:"device"`
}

// callLogin performs POST /v4/user/login and returns the JWT.
func (a *app) callLogin(ctx context.Context, payload loginPayload, device deviceInfo) (string, error) {
	up := a.upstream()
	if payload.Redirect == "" {
		payload.Redirect = "https://www.letsmagic.cn/super"
	}
	if payload.StateCode == "" {
		payload.StateCode = "+86"
	}
	payload.Device = device
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	headers := map[string]string{
		"Content-Type": "application/json",
		"language":     "zh_CN",
		"request-id":   requestID(""),
	}
	status, header, respBody, errCall := up.call(ctx, http.MethodPost, up.cfg.AccountBaseURL+"/v4/user/login", headers, body)
	if errCall != nil {
		return "", &failure{Status: http.StatusBadGateway, Message: "POST /v4/user/login: " + errCall.Error(), Retryable: true}
	}
	data, errUnwrap := up.unwrapAsset(status, header, respBody, "POST /v4/user/login")
	if errUnwrap != nil {
		return "", errUnwrap
	}
	var parsed loginResponse
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.AccessToken == "" {
		return "", &failure{Status: http.StatusBadGateway, Message: "登录响应缺少 access_token：" + truncateText(string(data), 200)}
	}
	return parsed.AccessToken, nil
}

// generateCaptcha asks the account service for a rotate captcha.
type captchaChallenge struct {
	Token string `json:"token"`
	Image string `json:"image"`
}

func (a *app) generateCaptcha(ctx context.Context, phone, stateCode string) (captchaChallenge, error) {
	var out captchaChallenge
	up := a.upstream()
	if stateCode == "" {
		stateCode = "+86"
	}
	body, _ := json.Marshal(map[string]string{
		"account_type": "mobile",
		"mobile":       phone,
		"state_code":   stateCode,
	})
	status, header, respBody, err := up.call(ctx, http.MethodPost, up.cfg.AccountBaseURL+"/captcha/generate", map[string]string{
		"Content-Type": "application/json",
		"language":     "zh_CN",
	}, body)
	if err != nil {
		return out, &failure{Status: http.StatusBadGateway, Message: "POST /captcha/generate: " + err.Error(), Retryable: true}
	}
	data, errUnwrap := up.unwrapAsset(status, header, respBody, "POST /captcha/generate")
	if errUnwrap != nil {
		return out, errUnwrap
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("解析验证码失败：%w", err)
	}
	return out, nil
}

// captchaAngle reproduces the client's angle obfuscation:
//
//	n = Date.now() % 90000 + 10000
//	Se = (52249517 * (angle + n) + 5976906165) mod 2^32
//	field = fmt.Sprintf("%d%d", Se, n)
//
// The server recovers the angle from the trailing 5 digits.
func captchaAngle(angle int) string {
	return captchaAngleWithNonce(angle, time.Now().UnixMilli()%90000+10000)
}

// captchaAngleWithNonce is the deterministic core of captchaAngle.
func captchaAngleWithNonce(angle int, n int64) string {
	norm := angle % 360
	if norm < 0 {
		norm += 360
	}
	acc := new(big.Int).Mul(big.NewInt(52249517), big.NewInt(int64(norm)+n))
	acc.Add(acc, big.NewInt(5976906165))
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 32), big.NewInt(1))
	acc.And(acc, mask)
	return fmt.Sprintf("%s%d", acc.String(), n)
}

type captchaVerifyResult struct {
	Verified      bool   `json:"verified"`
	BusinessToken string `json:"business_token"`
}

func (a *app) verifyCaptcha(ctx context.Context, token string, angle int, phone, stateCode string) (captchaVerifyResult, error) {
	var out captchaVerifyResult
	up := a.upstream()
	if stateCode == "" {
		stateCode = "+86"
	}
	body, _ := json.Marshal(map[string]any{
		"token":        token,
		"angle":        captchaAngle(angle),
		"account_type": "mobile",
		"mobile":       phone,
		"state_code":   stateCode,
	})
	status, header, respBody, err := up.call(ctx, http.MethodPost, up.cfg.AccountBaseURL+"/captcha/verify", map[string]string{
		"Content-Type": "application/json",
		"language":     "zh_CN",
	}, body)
	if err != nil {
		return out, &failure{Status: http.StatusBadGateway, Message: "POST /captcha/verify: " + err.Error(), Retryable: true}
	}
	data, errUnwrap := up.unwrapAsset(status, header, respBody, "POST /captcha/verify")
	if errUnwrap != nil {
		return out, errUnwrap
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("解析验证码结果失败：%w", err)
	}
	return out, nil
}

// sendSMSCode asks for the login SMS. It requires the captcha business token.
func (a *app) sendSMSCode(ctx context.Context, phone, stateCode, captchaToken string) error {
	up := a.upstream()
	if stateCode == "" {
		stateCode = "+86"
	}
	body, _ := json.Marshal(map[string]string{
		"type":       "account_login",
		"phone":      phone,
		"state_code": stateCode,
	})
	headers := map[string]string{
		"Content-Type":    "application/json",
		"language":        "zh_CN",
		"X-Captcha-Token": captchaToken,
	}
	status, header, respBody, err := up.call(ctx, http.MethodPost, up.cfg.AccountBaseURL+"/v4/user/send-sms", headers, body)
	if err != nil {
		return &failure{Status: http.StatusBadGateway, Message: "POST /v4/user/send-sms: " + err.Error(), Retryable: true}
	}
	_, errUnwrap := up.unwrapAsset(status, header, respBody, "POST /v4/user/send-sms")
	return errUnwrap
}

// findAccountForLogin matches an existing account by alias, phone or user id so
// that signing in again renews it in place instead of adding a parallel entry.
func (a *app) findAccountForLogin(alias, phone, authorization string) *credential {
	if alias != "" {
		if account := a.pool.get(accountIDForAlias(alias)); account != nil {
			return account
		}
	}
	phone = strings.TrimSpace(phone)
	userID := jwtSubject(authorization)
	for _, account := range a.pool.snapshot() {
		account.mu.Lock()
		match := (phone != "" && account.Phone == phone) || (userID != "" && account.UserID == userID)
		account.mu.Unlock()
		if match {
			return account
		}
	}
	return nil
}

// adoptLogin turns a fresh JWT into a pool account.
func (a *app) adoptLogin(ctx context.Context, authorization, alias, phone string) (*credential, error) {
	authorization = strings.TrimSpace(authorization)
	if authorization == "" {
		return nil, fmt.Errorf("登录响应没有 access_token")
	}
	org, err := a.resolveOrganization(ctx, authorization)
	if err != nil {
		return nil, err
	}
	// Signing in again is the only way to renew the 30-day login token, so an
	// existing account for the same person is reused instead of duplicated.
	if existing := a.findAccountForLogin(alias, phone, authorization); existing != nil {
		alias = existing.Alias
	}
	if alias == "" {
		alias = "login-" + time.Now().In(serverZone).Format("0102-150405")
	}
	account := &credential{
		ID:               accountIDForAlias(alias),
		Alias:            sanitizeAlias(alias),
		Authorization:    authorization,
		OrganizationCode: org,
		Device:           newDevice("login:" + alias),
		Priority:         5,
		Source:           "login",
		Phone:            phone,
		UpdatedAt:        time.Now().UnixMilli(),
	}
	if existing := a.pool.get(account.ID); existing != nil {
		existing.mu.Lock()
		// A re-login renews the login token, so everything the user configured on
		// the account has to survive the swap.
		account.APIKey = existing.APIKey
		account.RefreshToken = existing.RefreshToken
		account.APIKeyExpiresAt = existing.APIKeyExpiresAt
		account.RefreshTokenExpiresAt = existing.RefreshTokenExpiresAt
		account.Device = existing.Device
		account.Priority = existing.Priority
		account.Note = existing.Note
		account.Disabled = existing.Disabled
		account.Nickname = existing.Nickname
		account.UserID = existing.UserID
		if account.Phone == "" {
			account.Phone = existing.Phone
		}
		if account.Password == "" {
			account.Password = existing.Password
			account.AutoRelogin = existing.AutoRelogin
		}
		existing.mu.Unlock()
	}
	account.bucket = newTokenBucket(a.currentConfig().RPM, a.currentConfig().Burst)
	a.pool.put(account)
	a.persistAccounts()
	a.publishAccount(account, true)
	a.refreshAccountInfo(account)
	// The login gift is the client's own reward hook; trigger it immediately.
	if a.currentConfig().DailyPointsEnabled {
		a.spawn("points", func() {
			giftCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if gift, err := a.claimLoginGift(giftCtx, account); err == nil && gift.Granted {
				a.log.info("points", fmt.Sprintf("账号 %s 登录奖励到账 %s", account.label(), formatPoints(gift.PointAmount.Float64())))
			}
		})
	}
	return account, nil
}

// refreshAccountInfo pulls the user profile (masked phone, nickname, id) so the
// console can show who an account belongs to.
func (a *app) refreshAccountInfo(account *credential) {
	if account == nil || account.Authorization == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	up := a.upstream()
	headers := up.assetHeaders(account.Authorization, account.OrganizationCode, "")
	status, header, payload, err := up.call(ctx, http.MethodGet, up.cfg.AccountBaseURL+"/v4/users/info", headers, nil)
	if err != nil {
		return
	}
	data, errUnwrap := up.unwrapAsset(status, header, payload, "GET /v4/users/info")
	if errUnwrap != nil {
		return
	}
	var info struct {
		ID       flexString `json:"id"`
		RealName string     `json:"real_name"`
		NickName string     `json:"nick_name"`
		Phone    string     `json:"phone"`
	}
	if json.Unmarshal(data, &info) != nil {
		return
	}
	account.mu.Lock()
	account.UserID = info.ID.String()
	if info.NickName != "" {
		account.Nickname = info.NickName
	} else if info.RealName != "" {
		account.Nickname = info.RealName
	}
	// Upstream reports a masked phone (138****8000). Never let that overwrite a
	// real number: automatic re-login needs the one you can type.
	if info.Phone != "" && !isRealPhone(account.Phone) {
		account.Phone = info.Phone
	}
	account.mu.Unlock()
	a.persistAccounts()
}

// The login token cannot be refreshed upstream, so the only automatic renewal
// is signing in again. Accounts that opted in (see AutoRelogin) keep their
// password for exactly this purpose.
const (
	// reloginLead starts trying this long before the token dies, so the account
	// never actually goes dark when the password still works.
	reloginLead = 48 * time.Hour
	// reloginInterval throttles retries per account.
	reloginInterval = 6 * time.Hour
	// reloginMaxFails stops the loop from hammering a bad password: after this
	// many consecutive failures it only retries once a day.
	reloginMaxFails = 3
)

// reloginDue decides whether an account should be signed in again right now.
func reloginDue(c *credential, now time.Time) (phone, password string, due bool) {
	if c == nil {
		return "", "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	phone = strings.TrimSpace(c.Phone)
	password = c.Password
	switch {
	case !c.AutoRelogin || password == "" || !isRealPhone(phone):
		return "", "", false
	case c.Disabled || c.Dead:
		return "", "", false
	case c.JWTExpiresAt <= 0:
		// No local opinion on the deadline: never guess.
		return "", "", false
	case now.Before(time.UnixMilli(c.JWTExpiresAt).Add(-reloginLead)):
		return "", "", false
	}
	interval := reloginInterval
	if c.reloginFails >= reloginMaxFails {
		interval = 24 * time.Hour
	}
	if !c.reloginAt.IsZero() && now.Sub(c.reloginAt) < interval {
		return "", "", false
	}
	return phone, password, true
}

// autoRelogin renews expiring login tokens for accounts that stored a password.
func (a *app) autoRelogin(ctx context.Context) {
	now := time.Now()
	for _, account := range a.pool.snapshot() {
		phone, password, due := reloginDue(account, now)
		if !due {
			continue
		}
		a.reloginAccount(ctx, account, phone, password, account.Alias)
	}
}

// reloginAccount signs in again with the stored password and swaps the account
// in place. Returns true when the login token was renewed.
func (a *app) reloginAccount(ctx context.Context, account *credential, phone, password, alias string) bool {
	account.mu.Lock()
	account.reloginAt = time.Now()
	account.mu.Unlock()

	loginCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	device := newDevice("relogin:" + phone)
	token, err := a.callLogin(loginCtx, loginPayload{Phone: phone, Password: password, Type: "phone_password"}, device)
	if err != nil {
		account.mu.Lock()
		account.reloginFails++
		fails := account.reloginFails
		account.mu.Unlock()
		message := fmt.Sprintf("账号 %s 自动重登失败（连续 %d 次）：%s", alias, fails, asFailure(err).Message)
		if fails >= reloginMaxFails {
			message += "；已暂停自动重登，请手动重登或检查密码"
		}
		a.log.warn("login", message)
		return false
	}
	renewed, err := a.adoptLogin(loginCtx, token, alias, phone)
	if err != nil {
		account.mu.Lock()
		account.reloginFails++
		account.mu.Unlock()
		a.log.warn("login", fmt.Sprintf("账号 %s 自动重登后写入失败：%s", alias, asFailure(err).Message))
		return false
	}
	renewed.mu.Lock()
	renewed.reloginFails = 0
	until := renewed.JWTExpiresAt
	renewed.mu.Unlock()
	a.persistAccounts()
	a.log.info("login", fmt.Sprintf("账号 %s 登录令牌已自动续期，有效期至 %s",
		alias, time.UnixMilli(until).In(serverZone).Format("2006-01-02 15:04")))
	return true
}

// isRealPhone tells a dialable number from the masked one upstream reports.
func isRealPhone(phone string) bool {
	phone = strings.TrimSpace(phone)
	if len(phone) < 6 || len(phone) > 20 {
		return false
	}
	for _, r := range phone {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
