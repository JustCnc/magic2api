package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// execute handles a non-streaming chat-completions call.
func (a *app) execute(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	if format := normalizeFormat(req.Format); format != "" && format != formatChat {
		return upstreamError("magic2api 只接受 chat-completions 请求", http.StatusBadRequest, false), nil
	}
	cfg := a.currentConfig()
	upstreamModel, ok := cfg.upstreamModel(firstNonEmpty(payloadModel(req.Payload), req.Model))
	if !ok || upstreamModel == "" {
		return upstreamError("模型不属于 magic2api："+firstNonEmpty(req.Model, payloadModel(req.Payload)), http.StatusNotFound, false), nil
	}
	body, errBody := rewriteModel(req.Payload, upstreamModel)
	if errBody != nil {
		return upstreamError("请求体无效："+errBody.Error(), http.StatusBadRequest, false), nil
	}
	start := time.Now()
	result, account, f := a.dispatch(context.Background(), cfg, body, false, preferredAccountID(req.executorRequest))
	if f != nil {
		a.metrics.record(false)
		a.log.error("chat", fmt.Sprintf("model=%s HTTP=%d %s", upstreamModel, f.Status, f.Message))
		return upstreamError(f.Message, f.Status, f.Retryable), nil
	}
	elapsed := time.Since(start).Milliseconds()
	usage := parseUsage(result.body)
	entry := requestEntry{
		At:            start.UnixMilli(),
		Model:         cfg.qualifyModel(upstreamModel),
		UpstreamModel: upstreamModel,
		Status:        result.status,
		TotalMs:       elapsed,
		TTFTms:        elapsed,
	}
	if account != nil {
		entry.Account = account.label()
		entry.AccountID = account.ID
	}
	applyUsageToEntry(&entry, usage)
	seq := a.requests.add(entry)
	a.metrics.record(true)
	if usage != nil {
		a.metrics.tokensIn.Add(usage.PromptTokens)
		a.metrics.tokensOut.Add(usage.CompletionTokens)
	}
	a.log.info("chat", fmt.Sprintf("model=%s %dms bytes=%d", upstreamModel, elapsed, len(result.body)))
	if cfg.DebugDump {
		a.log.debugDump("chat", "response: "+truncateText(string(result.body), 800))
	}
	if cfg.PointsLogging && account != nil {
		go a.attachRequestPoints(seq, account, upstreamModel, start, time.Now())
	}
	return okEnvelope(executorResponse{
		Payload: result.body,
		Headers: http.Header{"Content-Type": []string{firstNonEmpty(result.contentType, "application/json")}},
	})
}

// executeStream forwards an SSE answer chunk by chunk through the host.
func (a *app) executeStream(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	if format := normalizeFormat(req.Format); format != "" && format != formatChat {
		return upstreamError("magic2api 只接受 chat-completions 请求", http.StatusBadRequest, false), nil
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	cfg := a.currentConfig()
	upstreamModel, ok := cfg.upstreamModel(firstNonEmpty(payloadModel(req.Payload), req.Model))
	if !ok || upstreamModel == "" {
		return upstreamError("模型不属于 magic2api："+firstNonEmpty(req.Model, payloadModel(req.Payload)), http.StatusNotFound, false), nil
	}
	body, errBody := rewriteModel(req.Payload, upstreamModel)
	if errBody != nil {
		return upstreamError("请求体无效："+errBody.Error(), http.StatusBadRequest, false), nil
	}
	start := time.Now()
	resp, account, f := a.openStream(context.Background(), cfg, body, preferredAccountID(req.executorRequest))
	if f != nil {
		a.metrics.record(false)
		a.log.error("stream", fmt.Sprintf("model=%s HTTP=%d %s", upstreamModel, f.Status, f.Message))
		return upstreamError(f.Message, f.Status, f.Retryable), nil
	}
	entry := requestEntry{
		At:            start.UnixMilli(),
		Model:         cfg.qualifyModel(upstreamModel),
		UpstreamModel: upstreamModel,
		Stream:        true,
		Status:        http.StatusOK,
	}
	if account != nil {
		entry.Account = account.label()
		entry.AccountID = account.ID
	}

	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		// A stream request answered with a plain completion: replay it as chunks.
		payload, errRead := readAllLimited(resp.Body, 64<<20)
		resp.Body.Close()
		if errRead != nil {
			a.metrics.record(false)
			return upstreamError("读取上游响应失败："+errRead.Error(), http.StatusBadGateway, true), nil
		}
		chunks := completionToChunks(payload)
		if len(chunks) == 0 {
			a.metrics.record(false)
			return upstreamError("上游返回了无法解析的响应体："+truncateText(string(payload), 300), http.StatusBadGateway, true), nil
		}
		usage := parseUsage(payload)
		go func() {
			for _, chunk := range chunks {
				if errEmit := a.emit(streamID, chunk); errEmit != nil {
					a.closeStream(streamID, errEmit.Error())
					return
				}
			}
			elapsed := time.Since(start).Milliseconds()
			entry.TotalMs = elapsed
			entry.TTFTms = elapsed
			applyUsageToEntry(&entry, usage)
			seq := a.requests.add(entry)
			a.metrics.record(true)
			a.metrics.streamed.Add(1)
			if cfg.PointsLogging && account != nil {
				go a.attachRequestPoints(seq, account, upstreamModel, start, time.Now())
			}
			a.closeStream(streamID, "")
		}()
		return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	idle := time.AfterFunc(time.Duration(cfg.RequestTimeoutSeconds)*time.Second, cancel)
	go func() {
		defer cancel()
		defer idle.Stop()
		defer resp.Body.Close()
		a.metrics.streamed.Add(1)
		stats := &streamStats{started: start}
		errPump := a.pumpStream(ctx, streamID, resp.Body, func() { idle.Reset(time.Duration(cfg.RequestTimeoutSeconds) * time.Second) }, stats)
		elapsed := time.Since(start).Milliseconds()
		entry.TotalMs = elapsed
		entry.TTFTms = stats.ttftMs
		applyUsageToEntry(&entry, stats.usage)
		if errPump != nil {
			fail := asFailure(errPump)
			entry.Status = fail.Status
			entry.Error = fail.Message
			if isClientGone(errPump) {
				// The downstream client hung up: not an upstream failure.
				entry.Status = 499
				entry.Error = "客户端中断"
			}
			a.requests.add(entry)
			a.metrics.record(false)
			a.log.error("stream", fmt.Sprintf("model=%s %dms %s", upstreamModel, elapsed, fail.Message))
			a.closeStream(streamID, fail.Message)
			return
		}
		seq := a.requests.add(entry)
		a.metrics.record(true)
		if stats.usage != nil {
			a.metrics.tokensIn.Add(stats.usage.PromptTokens)
			a.metrics.tokensOut.Add(stats.usage.CompletionTokens)
		}
		a.log.info("stream", fmt.Sprintf("model=%s %dms", upstreamModel, elapsed))
		if cfg.PointsLogging && account != nil {
			go a.attachRequestPoints(seq, account, upstreamModel, start, time.Now())
		}
		a.closeStream(streamID, "")
	}()
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

type chatResult struct {
	status      int
	body        []byte
	contentType string
}

// dispatch runs the account rotation loop for one non-streaming request.
func (a *app) dispatch(ctx context.Context, cfg Config, body []byte, stream bool, preferred string) (chatResult, *credential, *failure) {
	tried := make(map[string]bool)
	var lastFailure *failure
	attempts := cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		account := a.pool.choose(preferred, tried)
		if account == nil {
			break
		}
		tried[account.ID] = true
		if account.bucket != nil && !account.bucket.allow() {
			a.metrics.localLimit.Add(1)
			lastFailure = &failure{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("账号 %s 本地限流（%d rpm），请换号重试", account.label(), cfg.RPM), Retryable: true, RateLimit: true}
			continue
		}
		apiKey, errKey := a.accounts.ensureAPIKey(ctx, account)
		if errKey != nil {
			f := asFailure(errKey)
			if f.Auth {
				// A rejected JWT cannot be repaired by retrying this account.
				account.mu.Lock()
				account.Dead = false
				account.cooldownUntil = time.Now().Add(time.Duration(cfg.CooldownSeconds) * time.Second)
				account.mu.Unlock()
			}
			a.accounts.markFailure(account, f)
			lastFailure = f
			continue
		}
		result, f := a.chatWithRetry(ctx, account, apiKey, body, stream)
		if f == nil {
			a.accounts.markSuccess(account)
			return result, account, nil
		}
		a.accounts.markFailure(account, f)
		lastFailure = f
		if f.Dead {
			continue
		}
	}
	if lastFailure == nil {
		lastFailure = &failure{Status: http.StatusServiceUnavailable, Message: "没有可用账号（全部冷却、禁用或凭据失效）", Retryable: true}
	}
	return chatResult{}, nil, lastFailure
}

// openStream is dispatch for a streaming call: it returns the live response.
func (a *app) openStream(ctx context.Context, cfg Config, body []byte, preferred string) (*http.Response, *credential, *failure) {
	tried := make(map[string]bool)
	var lastFailure *failure
	attempts := cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		account := a.pool.choose(preferred, tried)
		if account == nil {
			break
		}
		tried[account.ID] = true
		if account.bucket != nil && !account.bucket.allow() {
			a.metrics.localLimit.Add(1)
			lastFailure = &failure{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("账号 %s 本地限流（%d rpm），请换号重试", account.label(), cfg.RPM), Retryable: true, RateLimit: true}
			continue
		}
		apiKey, errKey := a.accounts.ensureAPIKey(ctx, account)
		if errKey != nil {
			f := asFailure(errKey)
			a.accounts.markFailure(account, f)
			lastFailure = f
			continue
		}
		for retry := 0; retry < 2; retry++ {
			resp, errOpen := a.upstream().openChat(ctx, apiKey, body, true)
			if errOpen != nil {
				f := asFailure(errOpen)
				lastFailure = f
				if retry == 0 && f.Retryable {
					continue
				}
				a.accounts.markFailure(account, f)
				break
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				a.accounts.markSuccess(account)
				return resp, account, nil
			}
			payload, _ := readAllLimited(resp.Body, 1<<20)
			resp.Body.Close()
			if f := gatewayAuthFailure(resp.StatusCode, resp.Header, payload); f != nil {
				a.accounts.invalidate(account)
				if retry == 0 {
					if refreshed, errRefresh := a.accounts.ensureAPIKey(ctx, account); errRefresh == nil {
						apiKey = refreshed
						a.metrics.authRetry.Add(1)
						continue
					}
				}
				a.accounts.markFailure(account, f)
				lastFailure = f
				break
			}
			f := classifyChatError(resp.StatusCode, resp.Header, payload)
			a.accounts.markFailure(account, f)
			lastFailure = f
			if retry == 0 && !f.Retryable && !f.Auth {
				break
			}
			if retry == 0 && f.Retryable {
				continue
			}
			break
		}
	}
	if lastFailure == nil {
		lastFailure = &failure{Status: http.StatusServiceUnavailable, Message: "没有可用账号（全部冷却、禁用或凭据失效）", Retryable: true}
	}
	return nil, nil, lastFailure
}

// chatWithRetry performs one non-streaming call, retrying once on a fresh
// connection when the gateway answers something that is not JSON (WAF page,
// empty body).
func (a *app) chatWithRetry(ctx context.Context, account *credential, apiKey string, body []byte, stream bool) (chatResult, *failure) {
	for attempt := 0; attempt < 2; attempt++ {
		payload, header, err := a.upstream().chatOnce(ctx, apiKey, body)
		if err == nil {
			return chatResult{status: http.StatusOK, body: payload, contentType: header.Get("Content-Type")}, nil
		}
		f := asFailure(err)
		if f.Auth {
			a.accounts.invalidate(account)
			if attempt == 0 {
				if refreshed, errRefresh := a.accounts.ensureAPIKey(ctx, account); errRefresh == nil {
					apiKey = refreshed
					a.metrics.authRetry.Add(1)
					continue
				}
			}
			return chatResult{}, f
		}
		if attempt == 0 && isNonJSONFailure(f) {
			a.log.warn("chat", "上游返回非 JSON，换连接重试一次："+f.Message)
			continue
		}
		return chatResult{}, f
	}
	return chatResult{}, &failure{Status: http.StatusBadGateway, Message: "上游连续返回无法解析的响应", Retryable: true}
}

func isNonJSONFailure(f *failure) bool {
	return strings.Contains(f.Message, "content-type=") && strings.Contains(f.Message, "body=")
}

// rewriteModel replaces the model field in the payload, preserving everything
// else byte-for-byte as far as JSON ordering allows.
func rewriteModel(payload []byte, upstreamModel string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	doc["model"] = encoded
	return json.Marshal(doc)
}

func preferredAccountID(req executorRequest) string {
	if len(req.StorageJSON) > 0 {
		var in accountInput
		if json.Unmarshal(req.StorageJSON, &in) == nil {
			if alias := sanitizeAlias(in.Alias); alias != "" {
				return accountIDForAlias(alias)
			}
		}
	}
	id := strings.TrimSpace(req.AuthID)
	id = strings.TrimPrefix(id, providerKey+"-")
	return sanitizeAlias(id)
}

func applyUsageToEntry(entry *requestEntry, usage *usageInfo) {
	if usage == nil {
		return
	}
	entry.PromptTokens = usage.PromptTokens
	entry.OutputTokens = usage.CompletionTokens
	entry.CachedTokens = usage.CachedTokens
}

type streamStats struct {
	started time.Time
	ttftMs  int64
	usage   *usageInfo
	model   string
}

// pumpStream forwards each upstream event as a bare JSON payload; the host adds
// the "data: " framing and its own [DONE] terminator.
func (a *app) pumpStream(ctx context.Context, streamID string, body io.Reader, touch func(), stats *streamStats) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	sawFinish := false
	sawToolCall := false
	for scanner.Scan() {
		touch()
		line := bytes.TrimRight(scanner.Bytes(), "\r")
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(value) == 0 {
			continue
		}
		if bytes.Equal(value, []byte("[DONE]")) {
			if !sawFinish {
				a.emitFinish(streamID, stats, sawToolCall, "")
			}
			return nil
		}
		if usage := parseUsage(value); usage != nil && usage.PromptTokens+usage.CompletionTokens > 0 {
			stats.usage = usage
		}
		if bytes.Contains(value, []byte(`"finish_reason":"`)) && !bytes.Contains(value, []byte(`"finish_reason":null`)) {
			sawFinish = true
		}
		if bytes.Contains(value, []byte(`"tool_calls"`)) {
			sawToolCall = true
		}
		if stats.ttftMs == 0 && (bytes.Contains(value, []byte(`"content":"`)) || bytes.Contains(value, []byte(`"reasoning_content":"`))) {
			stats.ttftMs = time.Since(stats.started).Milliseconds()
		}
		chunk := append([]byte(nil), value...)
		if err := a.emit(streamID, chunk); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		if !sawFinish {
			a.emitFinish(streamID, stats, sawToolCall, "上游流中断："+err.Error())
		}
		return err
	}
	if !sawFinish {
		a.emitFinish(streamID, stats, sawToolCall, "")
	}
	return nil
}

// emitFinish synthesises the closing chunk when the upstream ended without a
// finish_reason: the host answers 502 otherwise.
func (a *app) emitFinish(streamID string, stats *streamStats, toolCall bool, errMsg string) {
	reason := "stop"
	if toolCall {
		reason = "tool_calls"
	}
	chunk := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if stats != nil && stats.model != "" {
		chunk["model"] = stats.model
	}
	if errMsg != "" {
		chunk["magic2api_error"] = errMsg
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	_ = a.emit(streamID, raw)
}

func (a *app) emit(streamID string, payload []byte) error {
	_, err := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func (a *app) closeStream(streamID, errMsg string) {
	_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: errMsg})
}

// completionToChunks replays a non-streaming completion as SSE chunks.
func completionToChunks(payload []byte) [][]byte {
	var completion struct {
		ID      string  `json:"id"`
		Model   string  `json:"model"`
		Created flexInt `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &completion); err != nil || len(completion.Choices) == 0 {
		return nil
	}
	choice := completion.Choices[0]
	build := func(delta map[string]any, finish string, usage json.RawMessage) []byte {
		chunk := map[string]any{
			"id":      completion.ID,
			"object":  "chat.completion.chunk",
			"created": completion.Created.Int64(),
			"model":   completion.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if len(usage) > 0 {
			chunk["usage"] = json.RawMessage(usage)
		}
		raw, _ := json.Marshal(chunk)
		return raw
	}
	var chunks [][]byte
	chunks = append(chunks, build(map[string]any{"role": "assistant"}, "", nil))
	if choice.Message.ReasoningContent != "" {
		chunks = append(chunks, build(map[string]any{"reasoning_content": choice.Message.ReasoningContent}, "", nil))
	}
	if choice.Message.Content != "" {
		chunks = append(chunks, build(map[string]any{"content": choice.Message.Content}, "", nil))
	}
	if len(choice.Message.ToolCalls) > 0 {
		var calls []any
		if json.Unmarshal(choice.Message.ToolCalls, &calls) == nil && len(calls) > 0 {
			chunks = append(chunks, build(map[string]any{"tool_calls": calls}, "", nil))
		}
	}
	finish := choice.FinishReason
	if finish == "" {
		finish = "stop"
	}
	chunks = append(chunks, build(map[string]any{}, finish, completion.Usage))
	return chunks
}

// isClientGone reports whether a stream error came from the downstream side.
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "host callback") || strings.Contains(message, "context canceled") || strings.Contains(message, "EOF")
}

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, limit)); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

var _ = sync.Mutex{}
