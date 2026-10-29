package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type catalogModel struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name,omitempty"`
	Category      string   `json:"category,omitempty"`
	ContextLength int64    `json:"context_length,omitempty"`
	MaxOutput     int64    `json:"max_output_tokens,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Created       int64    `json:"created,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	ModelType     string   `json:"model_type,omitempty"`
}

type catalogSnapshot struct {
	Models    []catalogModel `json:"models"`
	FetchedAt time.Time      `json:"fetched_at"`
	Source    string         `json:"source"`
}

func (a *app) snapshotCatalog() catalogSnapshot {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	return a.catalog
}

func (a *app) setCatalog(snapshot catalogSnapshot) {
	a.catalogMu.Lock()
	a.catalog = snapshot
	a.catalogMu.Unlock()
}

func (a *app) loadCachedCatalog() (catalogSnapshot, bool) {
	store := a.stateStore()
	if store == nil {
		return catalogSnapshot{}, false
	}
	var snapshot catalogSnapshot
	ok, err := store.readJSON("models.json", &snapshot)
	if err != nil || !ok || len(snapshot.Models) == 0 {
		return catalogSnapshot{}, false
	}
	snapshot.Source = "cache"
	return snapshot, true
}

// refreshCatalog pulls the authoritative list from /v1/models and enriches it
// with the platform catalogue when that call is available.
func (a *app) refreshCatalog(ctx context.Context) error {
	cfg := a.currentConfig()
	account := a.pool.choose("", nil)
	if account == nil {
		return fmt.Errorf("没有可用账号，无法拉取模型目录")
	}
	apiKey, err := a.accounts.ensureAPIKey(ctx, account)
	if err != nil {
		return err
	}
	models, err := a.upstream().listModels(ctx, apiKey)
	if err != nil {
		if f := asFailure(err); f.Auth {
			a.accounts.invalidate(account)
			if retryKey, retryErr := a.accounts.ensureAPIKey(ctx, account); retryErr == nil {
				models, err = a.upstream().listModels(ctx, retryKey)
			}
		}
		if err != nil {
			return err
		}
	}
	meta := a.fetchModelMetadata(ctx, account)
	deny := make(map[string]bool, len(cfg.ModelDenylist))
	for _, id := range cfg.ModelDenylist {
		deny[strings.TrimSpace(id)] = true
	}
	out := make([]catalogModel, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || deny[id] {
			continue
		}
		entry := catalogModel{ID: id, Created: m.Created.Int64(), Kind: m.Object}
		if extra, ok := meta[id]; ok {
			if isEmbeddingModel(id, extra.ModelType) {
				continue
			}
			entry.DisplayName = extra.DisplayName
			entry.Category = extra.Category
			entry.ContextLength = extra.ContextLength
			entry.MaxOutput = extra.MaxOutput
			entry.Capabilities = extra.Capabilities
			entry.ModelType = extra.ModelType
		}
		out = append(out, entry)
	}
	previous := a.snapshotCatalog()
	snapshot := catalogSnapshot{Models: out, FetchedAt: time.Now(), Source: "remote"}
	a.setCatalog(snapshot)
	if store := a.stateStore(); store != nil {
		if err := store.writeJSON("models.json", snapshot); err != nil {
			a.log.warn("catalog", "缓存模型目录失败："+err.Error())
		}
	}
	if !sameModelSet(previous.Models, out) {
		a.republishAccounts()
	}
	a.log.info("catalog", fmt.Sprintf("模型目录已更新：%d 个模型", len(out)))
	return nil
}

type modelMeta struct {
	DisplayName   string
	Category      string
	ModelType     string
	ContextLength int64
	MaxOutput     int64
	Capabilities  []string
}

// fetchModelMetadata reads /api/v1/platform/models for display names, context
// windows and capabilities. Failure is not fatal: the gateway list is enough.
func (a *app) fetchModelMetadata(ctx context.Context, account *credential) map[string]modelMeta {
	out := make(map[string]modelMeta)
	up := a.upstream()
	data, err := up.callAsset(ctx, "GET", "/api/v1/platform/models", account.Authorization, account.OrganizationCode, nil)
	if err != nil {
		return out
	}
	var payload struct {
		List []struct {
			ModelID      string   `json:"model_id"`
			Name         string   `json:"name"`
			Category     string   `json:"category"`
			ModelType    string   `json:"model_type"`
			MaxTokens    flexInt  `json:"max_tokens"`
			MaxOutput    flexInt  `json:"max_output_tokens"`
			Capabilities []string `json:"capabilities"`
		} `json:"list"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return out
	}
	for _, item := range payload.List {
		if item.ModelID == "" {
			continue
		}
		out[item.ModelID] = modelMeta{
			DisplayName:   item.Name,
			Category:      item.Category,
			ModelType:     item.ModelType,
			ContextLength: item.MaxTokens.Int64(),
			MaxOutput:     item.MaxOutput.Int64(),
			Capabilities:  item.Capabilities,
		}
	}
	return out
}

// registeredModels renders the catalogue in the host's shape.
func (a *app) registeredModels() []modelInfo {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	out := make([]modelInfo, 0, len(snapshot.Models))
	for _, m := range snapshot.Models {
		if m.Kind != "" && m.Kind != "model" {
			// Image/embedding entries cannot serve chat completions.
			continue
		}
		if m.Category != "" && m.Category != "llm" && m.Category != "vlm" {
			continue
		}
		if isEmbeddingModel(m.ID, m.ModelType) {
			continue
		}
		info := modelInfo{
			ID:          cfg.qualifyModel(m.ID),
			Object:      "model",
			Created:     m.Created,
			OwnedBy:     "magic",
			DisplayName: firstNonEmpty(m.DisplayName, m.ID),
			Name:        m.ID,
			Description: m.DisplayName,
			UserDefined: false,
		}
		if m.ContextLength > 0 {
			info.ContextLength = m.ContextLength
			info.InputTokenLimit = m.ContextLength
		}
		if m.MaxOutput > 0 {
			info.OutputTokenLimit = m.MaxOutput
			info.MaxCompletionTokens = m.MaxOutput
		}
		info.SupportedGenerationMethods = []string{"chat"}
		info.SupportedParameters = []string{"messages", "model", "stream", "temperature", "top_p", "max_tokens", "tools", "tool_choice", "response_format", "reasoning_effort"}
		info.SupportedInputModalities = []string{"text"}
		for _, capability := range m.Capabilities {
			if capability == "multi_modal" {
				info.SupportedInputModalities = []string{"text", "image"}
			}
		}
		info.SupportedOutputModalities = []string{"text"}
		out = append(out, info)
	}
	return out
}

// modelKnown reports whether the upstream id is in the current catalogue.
func (a *app) modelKnown(upstreamID string) bool {
	snapshot := a.snapshotCatalog()
	if len(snapshot.Models) == 0 {
		return true // an empty catalogue must not block every request
	}
	for _, m := range snapshot.Models {
		if m.ID == upstreamID {
			return true
		}
	}
	return false
}

// sameModelSet reports whether two catalogues expose the same model ids.
func sameModelSet(a, b []catalogModel) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]bool, len(a))
	for _, m := range a {
		index[m.ID] = true
	}
	for _, m := range b {
		if !index[m.ID] {
			return false
		}
	}
	return true
}

// republishAccounts rewrites every auth record so the host re-reads the model
// list attached to each credential. The host only asks for models when it loads
// an auth record, so a catalogue that arrived later is invisible otherwise.
func (a *app) republishAccounts() {
	for _, account := range a.pool.snapshot() {
		a.publishAccount(account, true)
	}
}

// isEmbeddingModel drops vectors: they cannot serve chat completions.
func isEmbeddingModel(id, modelType string) bool {
	lower := strings.ToLower(id + " " + modelType)
	return strings.Contains(lower, "embedding") || strings.Contains(lower, "嵌入")
}
