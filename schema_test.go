package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCaptchaAngleEncoding(t *testing.T) {
	// The server recovers the angle from the leading checksum and the trailing
	// five-digit nonce, so both halves have to line up.
	const angle = 137
	const nonce = 10000 + 4242
	encoded := captchaAngleWithNonce(angle, nonce)
	if len(encoded) < 6 {
		t.Fatalf("encoded angle too short: %q", encoded)
	}
	if tail := encoded[len(encoded)-5:]; tail != "14242" {
		t.Fatalf("nonce suffix = %q, want 14242", tail)
	}
	want := new(big.Int).Mul(big.NewInt(52249517), big.NewInt(angle+nonce))
	want.Add(want, big.NewInt(5976906165))
	want.And(want, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 32), big.NewInt(1)))
	if prefix := encoded[:len(encoded)-5]; prefix != want.String() {
		t.Fatalf("checksum = %q, want %s", prefix, want.String())
	}
}

func TestCaptchaAngleNormalisesRotation(t *testing.T) {
	// 360 and 0 describe the same rotation.
	if got, want := captchaAngleWithNonce(360, 12345), captchaAngleWithNonce(0, 12345); got != want {
		t.Fatalf("360deg = %q, 0deg = %q", got, want)
	}
	if got, want := captchaAngleWithNonce(-10, 12345), captchaAngleWithNonce(350, 12345); got != want {
		t.Fatalf("-10deg = %q, 350deg = %q", got, want)
	}
}

func TestFlexScalars(t *testing.T) {
	payload := []byte(`{
		"id": 974264387720527874,
		"amount": "-68",
		"total_points": "1220",
		"user_id": 12345,
		"topic_id": "",
		"score": 12.5,
		"nil": null
	}`)
	var decoded struct {
		ID     flexID     `json:"id"`
		Amount flexFloat  `json:"amount"`
		Total  flexFloat  `json:"total_points"`
		UserID flexString `json:"user_id"`
		Topic  flexString `json:"topic_id"`
		Score  flexFloat  `json:"score"`
		Nil    flexString `json:"nil"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ID.String() != "974264387720527874" {
		t.Fatalf("snowflake id lost precision: %q", decoded.ID.String())
	}
	if decoded.Amount.Float64() != -68 {
		t.Fatalf("amount = %v", decoded.Amount.Float64())
	}
	if decoded.Total.Float64() != 1220 {
		t.Fatalf("total = %v", decoded.Total.Float64())
	}
	if decoded.UserID.String() != "12345" {
		t.Fatalf("numeric user_id = %q", decoded.UserID.String())
	}
	if decoded.Topic.String() != "" || decoded.Nil.String() != "" {
		t.Fatalf("empty/null handling broken: %q %q", decoded.Topic.String(), decoded.Nil.String())
	}
	if decoded.Score.Float64() != 12.5 {
		t.Fatalf("score = %v", decoded.Score.Float64())
	}
}

func TestFlexIntAcceptsQuotedAndFloat(t *testing.T) {
	for _, raw := range []string{`42`, `"42"`, `42.0`, `"42.0"`} {
		var value flexInt
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if value.Int64() != 42 {
			t.Fatalf("%s decoded to %d", raw, value.Int64())
		}
	}
}

func TestModelPrefixRoundTrip(t *testing.T) {
	cfg := Config{ModelPrefix: "magic"}
	if got := cfg.qualifyModel("glm-5.3-flash"); got != "magic/glm-5.3-flash" {
		t.Fatalf("qualifyModel = %q", got)
	}
	if got := cfg.qualifyModel("magic/glm-5.3-flash"); got != "magic/glm-5.3-flash" {
		t.Fatalf("double prefix: %q", got)
	}
	if got, ok := cfg.upstreamModel("magic/glm-5.3-flash"); !ok || got != "glm-5.3-flash" {
		t.Fatalf("upstreamModel = %q %v", got, ok)
	}
	// A bare id is accepted, a foreign namespace is not.
	if got, ok := cfg.upstreamModel("glm-5.3-flash"); !ok || got != "glm-5.3-flash" {
		t.Fatalf("bare id = %q %v", got, ok)
	}
	if _, ok := cfg.upstreamModel("raccoon/whatever"); ok {
		t.Fatal("foreign namespace was accepted")
	}
}

func TestGatewayAuthFailureOnHTTP200(t *testing.T) {
	body := []byte(`{"code":403,"message":"token无效","data":{},"error":[]}`)
	f := gatewayAuthFailure(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, body)
	if f == nil || !f.Auth {
		t.Fatalf("expected an auth failure, got %#v", f)
	}
	// A real completion must never be mistaken for an auth error.
	completion := []byte(`{"id":"x","object":"chat.completion","choices":[]}`)
	if f := gatewayAuthFailure(http.StatusOK, nil, completion); f != nil {
		t.Fatalf("completion flagged as auth failure: %#v", f)
	}
}

func TestClassifyChatError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantRetry bool
		wantRate  bool
	}{
		{"unknown model", 400, `{"error":{"message":"不支持该模型","code":4001}}`, false, false},
		{"throttled", 429, `{"error":{"message":"too many"}}`, true, true},
		{"server error", 500, `oops`, true, false},
	}
	for _, tc := range cases {
		f := classifyChatError(tc.status, http.Header{"Content-Type": []string{"application/json"}}, []byte(tc.body))
		if f == nil {
			t.Fatalf("%s: nil failure", tc.name)
		}
		if f.Retryable != tc.wantRetry || f.RateLimit != tc.wantRate {
			t.Fatalf("%s: retryable=%v rate=%v", tc.name, f.Retryable, f.RateLimit)
		}
		if !strings.Contains(f.Message, "HTTP") {
			t.Fatalf("%s: message lacks the status: %q", tc.name, f.Message)
		}
	}
}

func TestDescribeRawResponseKeepsDiagnostics(t *testing.T) {
	message := describeRawResponse("GET /v1/models", 502, "text/html", []byte("<html>  blocked  </html>"))
	for _, want := range []string{"HTTP 502", "text/html", "<html> blocked </html>"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %q", want, message)
		}
	}
	empty := describeRawResponse("x", 200, "", nil)
	if !strings.Contains(empty, "<empty body>") || !strings.Contains(empty, "<no content-type>") {
		t.Fatalf("empty body description = %q", empty)
	}
}

func TestRewriteModelKeepsOtherFields(t *testing.T) {
	body := []byte(`{"model":"magic/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, err := rewriteModel(body, "glm-5.3-flash")
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["model"] != "glm-5.3-flash" {
		t.Fatalf("model = %v", decoded["model"])
	}
	if decoded["stream"] != true {
		t.Fatalf("stream lost: %v", decoded["stream"])
	}
	if _, ok := decoded["messages"]; !ok {
		t.Fatal("messages lost")
	}
}

func TestCompletionToChunks(t *testing.T) {
	payload := []byte(`{
		"id":"abc","model":"glm-5.3-flash","created":1793243629,
		"choices":[{"finish_reason":"tool_calls","message":{
			"content":"done","reasoning_content":"thinking",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}],
		"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	chunks := completionToChunks(payload)
	if len(chunks) < 4 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	last := string(chunks[len(chunks)-1])
	if !strings.Contains(last, `"finish_reason":"tool_calls"`) {
		t.Fatalf("last chunk lost finish_reason: %s", last)
	}
	if !strings.Contains(last, `"usage"`) {
		t.Fatalf("last chunk lost usage: %s", last)
	}
	if !strings.Contains(string(chunks[1]), "reasoning_content") {
		t.Fatalf("reasoning chunk missing: %s", chunks[1])
	}
}

func TestParseUsage(t *testing.T) {
	payload := []byte(`{"usage":{"prompt_tokens":17,"completion_tokens":32,"total_tokens":49,"prompt_tokens_details":{"cached_tokens":5}}}`)
	usage := parseUsage(payload)
	if usage == nil || usage.PromptTokens != 17 || usage.CompletionTokens != 32 || usage.CachedTokens != 5 {
		t.Fatalf("usage = %#v", usage)
	}
	if parseUsage([]byte(`{"choices":[]}`)) != nil {
		t.Fatal("payload without usage returned a value")
	}
}

func TestTokenBucket(t *testing.T) {
	bucket := newTokenBucket(60, 2)
	if !bucket.allow() || !bucket.allow() {
		t.Fatal("burst of 2 should pass")
	}
	if bucket.allow() {
		t.Fatal("third immediate call should be limited")
	}
}

func TestPoolChoosePrefersPriorityAndSkipsCooling(t *testing.T) {
	p := newPool()
	high := &credential{ID: "high", Alias: "high", Authorization: "a", OrganizationCode: "o", Priority: 10, UpdatedAt: 1}
	low := &credential{ID: "low", Alias: "low", Authorization: "a", OrganizationCode: "o", Priority: 1, UpdatedAt: 2}
	p.put(low)
	p.put(high)
	if got := p.choose("", nil); got == nil || got.ID != "high" {
		t.Fatalf("chose %v, want high", got)
	}
	high.cooldownUntil = time.Now().Add(time.Minute)
	if got := p.choose("", nil); got == nil || got.ID != "low" {
		t.Fatalf("cooling account was chosen: %v", got)
	}
	if got := p.choose("", map[string]bool{"high": true, "low": true}); got != nil {
		t.Fatalf("tried accounts were reused: %v", got)
	}
	// Cooling is a preference, not a veto: with nothing else left the account is
	// still tried, which keeps a single-account pool usable.
	if got := p.choose("", nil); got == nil || got.ID != "low" {
		t.Fatalf("cooling account outranked a warm one: %v", got)
	}
	// A disabled or dead account never wins.
	high.Dead = true
	low.Disabled = true
	if got := p.choose("", nil); got != nil {
		t.Fatalf("dead/disabled account chosen: %v", got)
	}
}

func TestServerTimeAndTTL(t *testing.T) {
	if _, ok := parseServerTime("2026-10-29 13:13:36"); !ok {
		t.Fatal("server timestamp was rejected")
	}
	if _, ok := parseServerTime("not a time"); ok {
		t.Fatal("garbage timestamp was accepted")
	}
	soon := time.Now().In(serverZone).Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	ttl := ttlFrom(soon, time.Minute)
	if ttl < 90*time.Minute || ttl > 130*time.Minute {
		t.Fatalf("ttl = %v", ttl)
	}
	// A timestamp in the past clamps to one minute instead of going negative.
	past := time.Now().In(serverZone).Add(-time.Hour).Format("2006-01-02 15:04:05")
	if ttl := ttlFrom(past, time.Hour); ttl != time.Minute {
		t.Fatalf("past ttl = %v", ttl)
	}
}

func TestIsEmbeddingModel(t *testing.T) {
	for _, id := range []string{"text-embedding-v3", "dmeta-embedding", "doubao-embedding-large"} {
		if !isEmbeddingModel(id, "") {
			t.Fatalf("%s not detected as embedding", id)
		}
	}
	if isEmbeddingModel("glm-5.3-flash", "大语言模型") {
		t.Fatal("chat model flagged as embedding")
	}
	if !isEmbeddingModel("foo", "嵌入模型") {
		t.Fatal("chinese embedding type not detected")
	}
}

func TestSameModelSet(t *testing.T) {
	a := []catalogModel{{ID: "x"}, {ID: "y"}}
	b := []catalogModel{{ID: "y"}, {ID: "x"}}
	if !sameModelSet(a, b) {
		t.Fatal("same set reported as different")
	}
	if sameModelSet(a, []catalogModel{{ID: "x"}}) {
		t.Fatal("different lengths reported as equal")
	}
	if sameModelSet(a, []catalogModel{{ID: "x"}, {ID: "z"}}) {
		t.Fatal("different ids reported as equal")
	}
}

func TestAuthIDOf(t *testing.T) {
	if got := authIDOf(hostAuthEntry{Name: "magic2api-main.json"}); got != "main" {
		t.Fatalf("authIDOf = %q", got)
	}
	if got := authIDOf(hostAuthEntry{Name: ""}); got != "" {
		t.Fatalf("empty name = %q", got)
	}
}

func TestRateGate(t *testing.T) {
	gate := newRateGate(2, time.Minute)
	if !gate.allow() || !gate.allow() {
		t.Fatal("first two calls should pass")
	}
	if gate.allow() {
		t.Fatal("third call should be blocked")
	}
}

func TestClaimModelRecordsMatchesModelAndWindow(t *testing.T) {
	a := &app{}
	started := time.Date(2026, 10, 29, 11, 14, 0, 0, serverZone)
	records := []pointsRecord{
		{ID: "1", BusinessType: "magic_model_consume", Description: "模型调用「glm-5.3-flash」", CreatedAt: "2026-10-29 11:14:00", Amount: -1},
		{ID: "2", BusinessType: "magic_model_consume", Description: "模型调用「other-model」", CreatedAt: "2026-10-29 11:14:00", Amount: -5},
		{ID: "3", BusinessType: "login_gift", Description: "每日登录赠送", CreatedAt: "2026-10-29 11:14:00", Amount: 200},
	}
	total, matched := a.claimModelRecords("acct", records, "glm-5.3-flash", started, started.Add(time.Second))
	if !matched || total != -1 {
		t.Fatalf("claim = %d %v", total, matched)
	}
	// A second call must not claim the same row again.
	if total, matched := a.claimModelRecords("acct", records, "glm-5.3-flash", started, started.Add(time.Second)); matched || total != 0 {
		t.Fatalf("row claimed twice: %d %v", total, matched)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]byte("enabled: true\npriority: 10\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.BaseURL != defaultBaseURL || cfg.AccountBaseURL != defaultAccountBaseURL {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.ModelPrefix != defaultModelPrefix || cfg.RPM != 30 || cfg.Burst != 20 {
		t.Fatalf("behaviour defaults: %+v", cfg)
	}
	if _, err := parseConfig([]byte("authorization: abc\n")); err == nil {
		t.Fatal("authorization without organization_code should fail")
	}
	cfg, err = parseConfig([]byte("authorization: abc\norganization_code: org\nrpm: 5\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.RPM != 5 || cfg.CredentialAlias != "default" {
		t.Fatalf("override: %+v", cfg)
	}
}

func TestNewUpstreamHasBothClients(t *testing.T) {
	// The streaming client has no whole-request timeout; a nil one panics the
	// host process on the first streaming call.
	u := newUpstream(Config{BaseURL: defaultBaseURL, RequestTimeoutSeconds: 30})
	if u.client == nil || u.stream == nil {
		t.Fatalf("clients not initialised: %#v", u)
	}
	if u.client.Timeout == 0 {
		t.Fatal("non-streaming client must keep the request timeout")
	}
	if u.stream.Timeout != 0 {
		t.Fatal("streaming client must not cap the whole request")
	}
}

func TestLoginSessionCompletion(t *testing.T) {
	a := &app{log: newLogRing(10)}
	session := a.newLoginSession()
	if session.State == "" {
		t.Fatal("empty state")
	}
	account := &credential{ID: "x", Alias: "x"}
	a.completeLoginSession(session.State, account)
	if a.loginSessionByState(session.State).Auth != account {
		t.Fatal("session was not completed")
	}
	// Completed sessions are consumed by pollLogin; an unknown state is a no-op.
	a.completeLoginSession("missing", account)
}

func TestResolvePointsAccountRejectsUnknownID(t *testing.T) {
	a := &app{log: newLogRing(10)}
	a.pool = newPool()
	a.pool.put(&credential{ID: "known", Alias: "known", Authorization: "t", OrganizationCode: "o"})
	if account, resp := a.resolvePointsAccount("known"); account == nil || resp != nil {
		t.Fatal("known id rejected")
	}
	// An explicit but unknown id must be an error, not a silent other account.
	if account, resp := a.resolvePointsAccount("ghost"); account != nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id fell back: account=%v resp=%v", account, resp)
	}
	// An empty id picks any usable account.
	if account, resp := a.resolvePointsAccount(""); account == nil || resp != nil {
		t.Fatal("empty id did not fall back")
	}
}

func TestParseSseEventsAcrossLines(t *testing.T) {
	// A single event split over two data lines must be emitted once, joined.
	a := &app{log: newLogRing(10)}
	var emitted [][]byte
	original := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if req, ok := payload.(streamEmitRequest); ok {
			emitted = append(emitted, append([]byte(nil), req.Payload...))
		}
		return json.RawMessage(`{}`), nil
	}
	defer func() { hostCall = original }()

	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\n" +
		"data: \"hi\"},\"finish_reason\":\"stop\"}],\"model\":\"m\"}\n\n" +
		"data: [DONE]\n\n")
	stats := &streamStats{started: time.Now()}
	if err := a.pumpStream(context.Background(), "s1", body, func() {}, stats); err != nil {
		t.Fatalf("pumpStream: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("expected one joined event, got %d: %q", len(emitted), emitted)
	}
	frame := string(emitted[0])
	if !strings.Contains(frame, "\"hi\"") || !strings.Contains(frame, "finish_reason") {
		t.Fatalf("joined frame lost data: %s", frame)
	}
	if stats.model != "" || stats.usage != nil {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestManagementPathNormalisation(t *testing.T) {
	// The console page is reachable from both mounts, and older builds produced a
	// doubled plugin prefix; every shape must land on the same handler.
	cases := map[string]string{
		"/ui":                                     "/ui",
		"/magic2api/ui":                           "/ui",
		"/v0/management/magic2api/ui":             "/ui",
		"/magic2api/magic2api/ui":                 "/ui",
		"/v0/resource/plugins/magic2api/ui":       "/ui",
		"/ui?state=abc":                           "/ui",
		"/magic2api/accounts/refresh":             "/accounts/refresh",
		"/v0/management/magic2api/points?id=live": "/points",
		"/": "/",
	}
	for input, want := range cases {
		if got := normalizeManagementPath(input); got != want {
			t.Errorf("normalizeManagementPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestExportAccountsSingleAndPool(t *testing.T) {
	a := &app{pool: newPool(), log: newLogRing(20)}
	a.pool.put(&credential{
		ID: "acc-1", Alias: "主账号", Authorization: "jwt-token", OrganizationCode: "h70imn51gp0f",
		APIKey: "mgw_abc", RefreshToken: "rt_abc", Priority: 2, Note: "备注",
		Device:    deviceInfo{ID: "dev-1", Name: "QA Device", OS: "Windows", OSVersion: "11"},
		UpdatedAt: 1700000000000,
	})

	// Whole pool: wrapped, provider-tagged, one record per account.
	all := a.handleManagement(managementRequest{Method: "GET", Path: "/magic2api/accounts/export"})
	if all.StatusCode != 200 {
		t.Fatalf("pool export status %d: %s", all.StatusCode, all.Body)
	}
	if got := all.Headers.Get("Content-Disposition"); !strings.Contains(got, `filename="magic2api-accounts-`) {
		t.Fatalf("pool export filename: %q", got)
	}
	var pooled struct {
		Provider string          `json:"provider"`
		Accounts []accountExport `json:"accounts"`
	}
	if err := json.Unmarshal(all.Body, &pooled); err != nil {
		t.Fatalf("pool export is not JSON: %v", err)
	}
	if pooled.Provider != providerKey || len(pooled.Accounts) != 1 {
		t.Fatalf("pool export payload: %+v", pooled)
	}
	record := pooled.Accounts[0]
	if record.Authorization != "jwt-token" || record.APIKey != "mgw_abc" || record.RefreshToken != "rt_abc" {
		t.Fatalf("tokens missing from export: %+v", record)
	}
	if record.Device.ID != "dev-1" || record.OrganizationCode != "h70imn51gp0f" {
		t.Fatalf("device/organisation missing from export: %+v", record)
	}

	// Single account: a bare record that /accounts/add accepts verbatim.
	one := a.handleManagement(managementRequest{Method: "GET", Path: "/accounts/export", Query: url.Values{"id": {"acc-1"}}})
	if one.StatusCode != 200 {
		t.Fatalf("single export status %d: %s", one.StatusCode, one.Body)
	}
	if got := one.Headers.Get("Content-Disposition"); !strings.Contains(got, "filename=") {
		t.Fatalf("single export filename: %q", got)
	}
	var input accountInput
	if err := json.Unmarshal(one.Body, &input); err != nil {
		t.Fatalf("single export is not importable JSON: %v", err)
	}
	if input.Authorization != "jwt-token" || input.Device.ID != "dev-1" || input.Note != "备注" {
		t.Fatalf("round-trip lost fields: %+v", input)
	}

	missing := a.handleManagement(managementRequest{Method: "GET", Path: "/accounts/export", Query: url.Values{"id": {"nope"}}})
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id status %d: %s", missing.StatusCode, missing.Body)
	}
}

func TestJWTExpiryAndSubject(t *testing.T) {
	// Shaped exactly like the token the account service issues: HS256 header and
	// base64 segments WITH padding (not RFC 7515 raw-url-safe).
	claims := `{"iss":974262954061238274,"exp":1795834730,"sub":"Json Web Token","cus":{"device_id":"f4adff"}}`
	token := base64.StdEncoding.EncodeToString([]byte(`{"alg":"HS256","type":"JWT"}`)) + "." +
		base64.StdEncoding.EncodeToString([]byte(claims)) + "." +
		strings.Repeat("0", 64)

	exp, ok := jwtExpiry(token)
	if !ok {
		t.Fatal("jwtExpiry failed on a padded token")
	}
	if got := exp.Unix(); got != 1795834730 {
		t.Fatalf("exp = %d, want 1795834730", got)
	}
	if got := jwtExpiresAtMillis(token); got != exp.UnixMilli() {
		t.Fatalf("jwtExpiresAtMillis = %d", got)
	}
	if got := jwtSubject(token); got != "974262954061238274" {
		t.Fatalf("jwtSubject = %q", got)
	}
	// A token without exp stays "unknown" instead of pretending to be expired.
	noExp := base64.StdEncoding.EncodeToString([]byte(`{"iss":5}`))
	if _, ok := jwtExpiry("h." + noExp + ".s"); ok {
		t.Fatal("a token without exp produced a deadline")
	}
	if _, ok := jwtExpiry("not-a-jwt"); ok {
		t.Fatal("garbage parsed as a JWT")
	}
	if jwtExpiresAtMillis("") != 0 {
		t.Fatal("empty token produced a deadline")
	}
}

func TestPoolPutDerivesJWTDeadline(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":123,"exp":1795834730}`))
	token := "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
	p := newPool()
	p.put(&credential{ID: "a", Alias: "a", Authorization: token, OrganizationCode: "o"})
	if got := p.get("a").JWTExpiresAt; got != 1795834730000 {
		t.Fatalf("JWTExpiresAt = %d", got)
	}

	// Replacing the token (re-login) must move the deadline, not keep the stale one.
	fresh := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":123,"exp":1798834730}`))
	p.put(&credential{ID: "a", Alias: "a", Authorization: "eyJhbGciOiJIUzI1NiJ9." + fresh + ".sig", OrganizationCode: "o"})
	if got := p.get("a").JWTExpiresAt; got != 1798834730000 {
		t.Fatalf("deadline not refreshed: %d", got)
	}
}

func TestExpiredJWTStopsTheAccount(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixMilli()
	future := time.Now().Add(time.Hour).UnixMilli()

	// Login token expired and the api key is gone: nobody can mint a new one.
	dead := &credential{ID: "dead", Authorization: "t", OrganizationCode: "o", JWTExpiresAt: past}
	if usable(dead) {
		t.Fatal("expired login token with no api key still reported usable")
	}
	// Login token expired but the cached api key is inside its own 2h window:
	// keep serving, upstream only checks the key.
	alive := &credential{ID: "alive", Authorization: "t", OrganizationCode: "o", JWTExpiresAt: past,
		APIKey: "mgw_x", APIKeyExpiresAt: time.Now().Add(90 * time.Minute).UnixMilli()}
	if !usable(alive) {
		t.Fatal("cached api key was dropped early")
	}
	if !usable(&credential{ID: "ok", Authorization: "t", OrganizationCode: "o", JWTExpiresAt: future}) {
		t.Fatal("valid login token reported unusable")
	}
	// Unparsable token keeps no local opinion and is left to upstream.
	if !usable(&credential{ID: "raw", Authorization: "t", OrganizationCode: "o"}) {
		t.Fatal("unknown expiry blocked the account")
	}
}

func TestEnsureAPIKeyReportsExpiredLogin(t *testing.T) {
	a := &app{pool: newPool(), log: newLogRing(10)}
	a.accounts = newAccountManager(a)
	account := &credential{ID: "old", Alias: "old", Authorization: "t", OrganizationCode: "o",
		JWTExpiresAt: time.Now().Add(-48 * time.Hour).UnixMilli()}
	_, err := a.accounts.ensureAPIKey(context.Background(), account)
	if err == nil {
		t.Fatal("expected a failure for an expired login token")
	}
	f := asFailure(err)
	if !f.Auth || f.Retryable {
		t.Fatalf("failure flags: %+v", f)
	}
	if !strings.Contains(f.Message, "过期") || !strings.Contains(f.Message, "重新登录") {
		t.Fatalf("message should mention re-login: %q", f.Message)
	}
	if f.Dead {
		t.Fatal("an expired login token should not mark the account dead")
	}
}

func TestReloginDue(t *testing.T) {
	now := time.Now()
	in72h := now.Add(72 * time.Hour).UnixMilli()
	in10h := now.Add(10 * time.Hour).UnixMilli()
	past := now.Add(-time.Hour).UnixMilli()

	base := func() *credential {
		return &credential{ID: "a", Alias: "a", Phone: "13800000000", Password: "pw", AutoRelogin: true,
			Authorization: "t", OrganizationCode: "o", JWTExpiresAt: in10h}
	}

	// Inside the lead window: sign in again.
	if _, _, due := reloginDue(base(), now); !due {
		t.Fatal("expiring token was not scheduled for re-login")
	}
	// Far from expiry: leave it alone.
	far := base()
	far.JWTExpiresAt = in72h
	if _, _, due := reloginDue(far, now); due {
		t.Fatal("token with 3 days left was re-logged too early")
	}
	// Opt-in only.
	off := base()
	off.AutoRelogin = false
	if _, _, due := reloginDue(off, now); due {
		t.Fatal("re-login ran without opt-in")
	}
	noPassword := base()
	noPassword.Password = ""
	if _, _, due := reloginDue(noPassword, now); due {
		t.Fatal("re-login ran without a stored password")
	}
	noPhone := base()
	noPhone.Phone = ""
	if _, _, due := reloginDue(noPhone, now); due {
		t.Fatal("re-login ran without a phone number")
	}
	// Disabled / dead accounts are the operator's decision.
	disabled := base()
	disabled.Disabled = true
	if _, _, due := reloginDue(disabled, now); due {
		t.Fatal("re-login ran for a disabled account")
	}
	// Throttle, then the slower retry after repeated failures.
	recent := base()
	recent.reloginAt = now.Add(-time.Hour)
	if _, _, due := reloginDue(recent, now); due {
		t.Fatal("re-login ignored the throttle")
	}
	recent.reloginAt = now.Add(-7 * time.Hour)
	if _, _, due := reloginDue(recent, now); !due {
		t.Fatal("re-login did not retry after the throttle window")
	}
	backoff := base()
	backoff.reloginFails = reloginMaxFails
	backoff.reloginAt = now.Add(-7 * time.Hour)
	if _, _, due := reloginDue(backoff, now); due {
		t.Fatal("a failing password kept retrying every 6h")
	}
	backoff.reloginAt = now.Add(-25 * time.Hour)
	if _, _, due := reloginDue(backoff, now); !due {
		t.Fatal("a failing password never retried again")
	}
	// Unknown deadline is never guessed.
	unknown := base()
	unknown.JWTExpiresAt = 0
	if _, _, due := reloginDue(unknown, now); due {
		t.Fatal("re-login ran without a known deadline")
	}
	_ = past
}

func TestUpdateAccountClearsStoredPassword(t *testing.T) {
	a := newTestApp()
	a.pool.put(&credential{ID: "acc", Alias: "acc", Authorization: "t", OrganizationCode: "o",
		Phone: "13800000000", Password: "pw", AutoRelogin: true})

	off := `{"id":"acc","auto_relogin":false}`
	if resp := a.handleManagement(managementRequest{Method: "POST", Path: "/accounts/update", Body: []byte(off)}); resp.StatusCode != 200 {
		t.Fatalf("update failed: %s", resp.Body)
	}
	account := a.pool.get("acc")
	account.mu.Lock()
	password, auto := account.Password, account.AutoRelogin
	account.mu.Unlock()
	if password != "" || auto {
		t.Fatalf("password survived the switch-off: %q %v", password, auto)
	}

	on := `{"id":"acc","password":"pw2","auto_relogin":true}`
	if resp := a.handleManagement(managementRequest{Method: "POST", Path: "/accounts/update", Body: []byte(on)}); resp.StatusCode != 200 {
		t.Fatalf("update failed: %s", resp.Body)
	}
	account.mu.Lock()
	password, auto = account.Password, account.AutoRelogin
	account.mu.Unlock()
	if password != "pw2" || !auto {
		t.Fatalf("opt-in not stored: %q %v", password, auto)
	}
}

// newTestApp builds the smallest app the management layer can run against:
// no host callbacks, in-memory state only.
func newTestApp() *app {
	a := &app{
		pool:      newPool(),
		log:       newLogRing(50),
		published: map[string]string{},
		requests:  newRequestLog(20),
	}
	a.accounts = newAccountManager(a)
	return a
}

func TestIsRealPhoneAndMask(t *testing.T) {
	for _, phone := range []string{"13800000000", "8613800000000"} {
		if !isRealPhone(phone) {
			t.Fatalf("%q should count as a dialable number", phone)
		}
	}
	for _, phone := range []string{"", "156****2610", "138 0000 0000", "abc"} {
		if isRealPhone(phone) {
			t.Fatalf("%q should not count as a dialable number", phone)
		}
	}
	if got := maskPhone("13800000000"); got != "138****0000" {
		t.Fatalf("maskPhone = %q", got)
	}
	// Already-masked input is passed through, so repeated updates do not mangle it.
	if got := maskPhone("156****2610"); got != "156****2610" {
		t.Fatalf("maskPhone masked twice: %q", got)
	}
}

func TestReloginSkipsMaskedPhone(t *testing.T) {
	now := time.Now()
	account := &credential{ID: "a", Alias: "a", Authorization: "t", OrganizationCode: "o",
		Phone: "156****2610", Password: "pw", AutoRelogin: true,
		JWTExpiresAt: now.Add(time.Hour).UnixMilli()}
	if _, _, due := reloginDue(account, now); due {
		t.Fatal("a masked phone cannot be used to sign in")
	}
	account.Phone = "15600002610"
	if _, _, due := reloginDue(account, now); !due {
		t.Fatal("a real phone should allow re-login")
	}
}

func TestAliasFromAuthFileName(t *testing.T) {
	cases := map[string]string{
		"magic2api-live.json":       "live",
		"magic2api-phone-8801.json": "phone-8801",
		"magic2api-main.live.json":  "main.live",
		"other-live.json":           "",
		"live.json":                 "",
		"":                          "",
	}
	for name, want := range cases {
		if got := aliasFromAuthFileName(name); got != want {
			t.Errorf("aliasFromAuthFileName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestUpdateAccountRenameKeepsID(t *testing.T) {
	a := newTestApp()
	a.pool.put(&credential{ID: "live", Alias: "live", Authorization: "t", OrganizationCode: "o", Priority: 5})

	body := []byte(`{"id":"live","alias":"main-live","priority":7,"note":"备注"}`)
	if resp := a.handleManagement(managementRequest{Method: "POST", Path: "/accounts/update", Body: body}); resp.StatusCode != 200 {
		t.Fatalf("rename failed: %s", resp.Body)
	}
	account := a.pool.get("live")
	if account == nil {
		t.Fatal("rename moved the account id, which would orphan the host credential file")
	}
	account.mu.Lock()
	alias, prio, note := account.Alias, account.Priority, account.Note
	account.mu.Unlock()
	if alias != "main-live" || prio != 7 || note != "备注" {
		t.Fatalf("rename result: %q %d %q", alias, prio, note)
	}
	if a.pool.get("main-live") != nil {
		t.Fatal("a second account was created under the new alias")
	}

	// A duplicate alias is refused rather than silently shadowing.
	a.pool.put(&credential{ID: "other", Alias: "other", Authorization: "t", OrganizationCode: "o"})
	dup := []byte(`{"id":"other","alias":"main-live"}`)
	if resp := a.handleManagement(managementRequest{Method: "POST", Path: "/accounts/update", Body: dup}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate alias accepted: %d %s", resp.StatusCode, resp.Body)
	}
	// Non-ASCII aliases are rejected with an explanation.
	bad := []byte(`{"id":"other","alias":"主号"}`)
	if resp := a.handleManagement(managementRequest{Method: "POST", Path: "/accounts/update", Body: bad}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad alias accepted: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestPickOrganizationShapes(t *testing.T) {
	// New accounts (web client): data is a flat array.
	flat := json.RawMessage(`[{"magic_id":"974331964651315201","organization_name":"",
		"magic_organization_code":"11ld4mob5ll7","third_platform_organization_code":"11ld4mob5ll7",
		"third_platform_type":"teamshare","teamshare_organization_code":"11ld4mob5ll7"}]`)
	if got, ok := pickOrganization(flat); !ok || got != "11ld4mob5ll7" {
		t.Fatalf("flat shape: %q %v", got, ok)
	}

	// Older accounts: nested under organizations, current flag wins.
	nested := json.RawMessage(`{"organizations":[
		{"organization_code":"first","magic_organization_code":"m-first","current":false},
		{"organization_code":"second","magic_organization_code":"m-second","current":true}],
		"organization_code":"outer","magic_organization_code":"m-outer"}`)
	if got, ok := pickOrganization(nested); !ok || got != "m-second" {
		t.Fatalf("nested shape: %q %v", got, ok)
	}

	// Only teamshare codes present.
	teamshare := json.RawMessage(`[{"teamshare_organization_code":"ts-only"}]`)
	if got, ok := pickOrganization(teamshare); !ok || got != "ts-only" {
		t.Fatalf("teamshare fallback: %q %v", got, ok)
	}

	// Single object.
	single := json.RawMessage(`{"organization_code":"solo"}`)
	if got, ok := pickOrganization(single); !ok || got != "solo" {
		t.Fatalf("single object: %q %v", got, ok)
	}

	// Nothing usable.
	for _, raw := range []string{`[]`, `{}`, `[{"organization_name":"x"}]`, `null`} {
		if got, ok := pickOrganization(json.RawMessage(raw)); ok {
			t.Fatalf("%s produced %q", raw, got)
		}
	}
}

func TestSumTodayRecordsSplitsIncomeAndExpense(t *testing.T) {
	income := []pointsRecord{
		{BusinessType: "login_gift", Amount: 200, CreatedAt: "2026-10-29 10:58:52"},
		{BusinessType: "onboarding_reward", Amount: 50, CreatedAt: "2026-10-29 16:01:24"},
		{BusinessType: "onboarding_reward", Amount: 100, CreatedAt: "2026-10-29 15:29:41"},
		{BusinessType: "international_register_rewards", Amount: 1000, CreatedAt: "2026-10-29 15:27:23"},
	}
	expense := []pointsRecord{
		{BusinessType: "magic_model_consume", Amount: -3, CreatedAt: "2026-10-29 16:02:00"},
		{BusinessType: "super_magic_task_consume", Amount: -6, CreatedAt: "2026-10-29 15:29:50"},
	}
	gift, task, consumed, claimedAt := sumTodayRecords(income, expense)
	if gift != 200 || task != 150 || consumed != -9 {
		t.Fatalf("gift=%v task=%v consumed=%v", gift, task, consumed)
	}
	if claimedAt != "2026-10-29 10:58:52" {
		t.Fatalf("claimedAt=%q", claimedAt)
	}
	// No login gift today: the console must show 未领取 rather than a zero claim.
	if gift, _, _, claimedAt := sumTodayRecords(nil, expense); gift != 0 || claimedAt != "" {
		t.Fatalf("empty income: gift=%v claimedAt=%q", gift, claimedAt)
	}
}

// The web bundle builds request-id as base36(now ms) | 12 hex random | 8 hex
// session fingerprint; anything else is a fingerprint of its own.
func TestRequestIDMatchesClientFormat(t *testing.T) {
	id := requestID("")
	if len(id) != 28 {
		t.Fatalf("length = %d, want 28: %q", len(id), id)
	}
	if id != strings.ToUpper(id) {
		t.Fatalf("not upper case: %q", id)
	}
	stamp, err := strconv.ParseInt(id[:8], 36, 64)
	if err != nil {
		t.Fatalf("time prefix %q is not base36: %v", id[:8], err)
	}
	if drift := time.Since(time.UnixMilli(stamp)); drift < -time.Second || drift > 5*time.Second {
		t.Fatalf("time prefix is %v off", drift)
	}
	if _, err := hex.DecodeString(id[8:20]); err != nil {
		t.Fatalf("random part %q is not hex: %v", id[8:20], err)
	}
	if tail := id[20:]; tail != sessionFingerprint() {
		t.Fatalf("fingerprint = %q, want %q", tail, sessionFingerprint())
	}
	if tail := requestID("DEADBEEFcafe"); tail[20:] != "DEADBEEF" {
		t.Fatalf("session override = %q", tail[20:])
	}
	if requestID("")[8:20] == requestID("")[8:20] {
		t.Fatal("random part repeats between calls")
	}
}

type expiryCase struct {
	Amount flexFloat
	At     time.Time
}

func balanceWithExpiring(cases ...expiryCase) pointsBalance {
	var balance pointsBalance
	for i, c := range cases {
		var quota struct {
			QuotaID         flexID    `json:"quota_id"`
			ExpiresAt       string    `json:"expires_at"`
			RemainingPoints flexFloat `json:"remaining_points"`
		}
		quota.QuotaID = flexID(strconv.Itoa(i + 1))
		quota.ExpiresAt = c.At.In(serverZone).Format("2006-01-02 15:04:05")
		quota.RemainingPoints = c.Amount
		balance.Expiring = append(balance.Expiring, quota)
	}
	return balance
}

func TestCompareExpiryRanksLapsingBalances(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tomorrow := expiryScoreOf(balanceWithExpiring(expiryCase{700, now.Add(24 * time.Hour)}), now)
	nextMonth := expiryScoreOf(balanceWithExpiring(expiryCase{100, now.Add(20 * 24 * time.Hour)}), now)
	farAway := expiryScoreOf(balanceWithExpiring(expiryCase{5000, now.Add(400 * 24 * time.Hour)}), now)
	empty := expiryScoreOf(pointsBalance{}, now)

	if tomorrow.Soon != 700 {
		t.Fatalf("soon = %v, want 700", tomorrow.Soon)
	}
	if farAway.Soon != 0 {
		t.Fatalf("points beyond the horizon counted as soon: %v", farAway.Soon)
	}
	// The soonest deadline wins even against a larger amount further out.
	if got := compareExpiry(tomorrow, nextMonth); got != 1 {
		t.Fatalf("tomorrow vs next month = %d, want 1", got)
	}
	// Anything at risk beats a balance that never lapses.
	if got := compareExpiry(nextMonth, empty); got != 1 {
		t.Fatalf("soon vs empty = %d, want 1", got)
	}
	if got := compareExpiry(nextMonth, farAway); got != 1 {
		t.Fatalf("soon vs far away = %d, want 1", got)
	}
	if got := compareExpiry(empty, empty); got != 0 {
		t.Fatalf("empty vs empty = %d, want 0", got)
	}
	// Same deadline: burn the bigger batch first.
	sameDay := expiryScoreOf(balanceWithExpiring(expiryCase{300, now.Add(48 * time.Hour)}), now)
	if got := compareExpiry(sameDay, tomorrow); got != -1 {
		t.Fatalf("smaller same-deadline batch = %d, want -1", got)
	}
}

func TestPoolChoosePrefersExpiringPointsAtSamePriority(t *testing.T) {
	now := time.Now()
	p := newPool()
	lapsing := &credential{ID: "lapsing", Alias: "lapsing", Authorization: "a", OrganizationCode: "o", Priority: 5, UpdatedAt: 1}
	fresh := &credential{ID: "fresh", Alias: "fresh", Authorization: "a", OrganizationCode: "o", Priority: 5, UpdatedAt: 2}
	p.put(lapsing)
	p.put(fresh)
	// Without scores the plain rotation order (most recent success) decides.
	if got := p.choose("", nil); got == nil || got.ID != "fresh" {
		t.Fatalf("chose %v, want fresh before scores exist", got)
	}
	scores := map[string]expiryScore{
		"lapsing": expiryScoreOf(balanceWithExpiring(expiryCase{900, now.Add(6 * time.Hour)}), now),
	}
	p.expiry = func(id string) expiryScore { return scores[id] }
	if got := p.choose("", nil); got == nil || got.ID != "lapsing" {
		t.Fatalf("chose %v, want the lapsing balance", got)
	}
	// A higher priority still wins over a lapsing balance further down.
	fresh.Priority = 6
	if got := p.choose("", nil); got == nil || got.ID != "fresh" {
		t.Fatalf("expiry outranked priority: %v", got)
	}
}

func TestPoolOverridesHostPickForLapsingBalance(t *testing.T) {
	now := time.Now()
	p := newPool()
	host := &credential{ID: "host", Alias: "host", Authorization: "a", OrganizationCode: "o", Priority: 5, UpdatedAt: 2}
	lapsing := &credential{ID: "lapsing", Alias: "lapsing", Authorization: "a", OrganizationCode: "o", Priority: 5, UpdatedAt: 1}
	senior := &credential{ID: "senior", Alias: "senior", Authorization: "a", OrganizationCode: "o", Priority: 9, UpdatedAt: 1}
	p.put(host)
	p.put(lapsing)
	p.put(senior)
	scores := map[string]expiryScore{
		"lapsing": expiryScoreOf(balanceWithExpiring(expiryCase{900, now.Add(4 * time.Hour)}), now),
		"senior":  expiryScoreOf(balanceWithExpiring(expiryCase{900, now.Add(1 * time.Hour)}), now),
	}
	p.expiry = func(id string) expiryScore { return scores[id] }
	// Same priority, sooner deadline: the host's pick is overridden.
	if got := p.choose("host", nil); got == nil || got.ID != "lapsing" {
		t.Fatalf("chose %v, want the lapsing peer", got)
	}
	// Stickiness survives when nobody at that priority lapses sooner.
	if got := p.choose("lapsing", nil); got == nil || got.ID != "lapsing" {
		t.Fatalf("chose %v, want the host pick kept", got)
	}
	// A higher-priority account is not hijacked by an expiry score.
	if got := p.choose("senior", nil); got == nil || got.ID != "senior" {
		t.Fatalf("chose %v, want the senior account", got)
	}
}

func TestApplySettingsPatchIgnoresDeploymentKeys(t *testing.T) {
	cfg, err := parseConfig([]byte("data_dir: " + t.TempDir()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	patched, err := applySettingsPatch(cfg, map[string]any{
		"rpm":                    float64(26),
		"prefer_expiring_points": true,
		"points_schedule_time":   "07:30",
		"data_dir":               "/tmp/evil",
		"base_url":               "https://evil.example",
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.RPM != 26 || !patched.PreferExpiringPoints || patched.PointsScheduleTime != "07:30" {
		t.Fatalf("console keys not applied: %+v", patched)
	}
	if patched.DataDir != cfg.DataDir || patched.BaseURL != cfg.BaseURL {
		t.Fatalf("deployment keys were rewritten: %s / %s", patched.DataDir, patched.BaseURL)
	}
}

func TestSaveSettingsSurvivesRestartAndResetDropsIt(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("data_dir: " + dir + "\nrpm: 30\nprefer_expiring_points: false\n")
	a := &app{log: newLogRing(50)}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.PreferExpiringPoints {
		t.Fatal("default should be off")
	}
	store, errStore := newStateStore(dir)
	if errStore != nil {
		t.Fatalf("state store: %v", errStore)
	}
	a.state = store
	a.saveSettings(map[string]any{"prefer_expiring_points": true, "rpm": float64(26), "junk": "x"})
	// A fresh start: the config file is unchanged, the saved edits win.
	restarted, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	restarted = loadSavedSettings(restarted, a.log)
	if !restarted.PreferExpiringPoints || restarted.RPM != 26 {
		t.Fatalf("saved settings were lost: prefer=%v rpm=%d", restarted.PreferExpiringPoints, restarted.RPM)
	}
	// Unknown keys never reach the file.
	var saved map[string]any
	if ok, _ := a.stateStore().readJSON(savedSettingsFile, &saved); !ok {
		t.Fatal("settings.json missing")
	}
	if _, ok := saved["junk"]; ok {
		t.Fatal("unlisted key was persisted")
	}
	// Reset drops the file and the config file rules again.
	if err := a.stateStore().remove(savedSettingsFile); err != nil {
		t.Fatalf("reset: %v", err)
	}
	fresh, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("reparse after reset: %v", err)
	}
	afterReset := loadSavedSettings(fresh, a.log)
	if afterReset.PreferExpiringPoints || afterReset.RPM != 30 {
		t.Fatalf("reset did not fall back: prefer=%v rpm=%d", afterReset.PreferExpiringPoints, afterReset.RPM)
	}
}

func newPersistentTestApp(t *testing.T, dir string) *app {
	t.Helper()
	store, err := newStateStore(dir)
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	a := &app{
		log:      newLogRing(50),
		metrics:  &metrics{},
		requests: newRequestLog(20),
		points:   make(map[string]*pointsCache),
		expiry:   make(map[string]expiryScore),
		state:    store,
	}
	a.cfg = defaultConfig()
	return a
}

func TestCredentialHealthSurvivesRestart(t *testing.T) {
	now := time.Now()
	c := &credential{ID: "acc", Authorization: "a", OrganizationCode: "o"}
	c.bucket = newTokenBucket(30, 20)
	c.failures = 2
	c.lastError = "HTTP 429"
	c.cooldownUntil = now.Add(45 * time.Second)
	c.lastSuccess = now.Add(-time.Minute)
	c.reloginFails = 1
	c.reloginAt = now.Add(-2 * time.Hour)
	if !c.bucket.allow() || !c.bucket.allow() {
		t.Fatal("bucket should start full")
	}

	raw, err := json.Marshal(c.exportSnapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded credential
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reloaded.bucket = newTokenBucket(30, 20)
	reloaded.restoreRuntime()

	if !reloaded.cooling() {
		t.Fatal("cooldown was lost")
	}
	if reloaded.failures != 2 || reloaded.lastError != "HTTP 429" {
		t.Fatalf("health lost: failures=%d err=%q", reloaded.failures, reloaded.lastError)
	}
	if reloaded.lastSuccess.IsZero() || reloaded.reloginFails != 1 || reloaded.reloginAt.IsZero() {
		t.Fatal("success/relogin state lost")
	}
	// 18 of the 20 burst tokens were spent; the rest refill normally.
	if tokens, _ := reloaded.bucket.snapshotState(); tokens < 18 || tokens > 20 {
		t.Fatalf("bucket not resumed: %v", tokens)
	}
}

func TestRuntimeStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := newPersistentTestApp(t, dir)
	a.metrics.record(true)
	a.metrics.tokensIn.Add(120)
	a.requests.add(requestEntry{Account: "live", Model: "magic/glm-5.1", Status: 200})
	a.log.info("chat", "hello")
	claimedRecords.claim("live:42")
	a.flushState()

	b := newPersistentTestApp(t, dir)
	b.loadRuntimeState()
	if got := b.metrics.requests.Load(); got != 1 {
		t.Fatalf("requests counter = %d, want 1", got)
	}
	if got := b.metrics.tokensIn.Load(); got != 120 {
		t.Fatalf("tokens_in = %d, want 120", got)
	}
	if rows := b.requests.recent(5); len(rows) != 1 || rows[0].Account != "live" {
		t.Fatalf("request log not restored: %+v", rows)
	}
	// The restore itself logs a line, so look for the persisted one, not the count.
	restoredLog := false
	for _, entry := range b.log.recent(5) {
		if entry.Scope == "chat" && entry.Message == "hello" {
			restoredLog = true
		}
	}
	if !restoredLog {
		t.Fatalf("log ring not restored: %+v", b.log.recent(5))
	}
	// The claimed row must not be attributed twice after a restart.
	if claimedRecords.claim("live:42") {
		t.Fatal("claimed ledger row was reusable after restart")
	}
	// Sequence numbers keep growing so a late backfill still matches.
	if seq := b.requests.add(requestEntry{Account: "live"}); seq != 2 {
		t.Fatalf("seq = %d, want 2", seq)
	}
}

func TestPointsCacheSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := newPersistentTestApp(t, dir)
	a.cfg.PreferExpiringPoints = true
	cache := a.pointsFor("acc")
	cache.mu.Lock()
	cache.balance = balanceWithExpiring(expiryCase{420, time.Now().Add(6 * time.Hour)})
	cache.ok = true
	cache.at = time.Now()
	cache.mu.Unlock()
	a.savePointsCache()

	b := newPersistentTestApp(t, dir)
	b.cfg.PreferExpiringPoints = true
	b.loadPointsCache()
	balance, err := b.balance(context.Background(), &credential{ID: "acc"}, false)
	if err != nil {
		t.Fatalf("balance from cache: %v", err)
	}
	if balance.Expiring[0].RemainingPoints.Float64() != 420 {
		t.Fatalf("balance not restored: %+v", balance)
	}
	// The router is warm immediately: no refresh sweep needed for the score.
	if score := b.expiryOf("acc"); score.Soon != 420 || score.Earliest.IsZero() {
		t.Fatalf("expiry score not seeded: %+v", score)
	}
}
