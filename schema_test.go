package main

import (
	"encoding/json"
	"math/big"
	"net/http"
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
