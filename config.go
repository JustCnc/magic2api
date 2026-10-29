package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the plugin-owned subtree of plugins.configs.magic2api.
// Host-owned keys (enabled, priority) are ignored here.
type Config struct {
	// DataDir holds accounts.json, the model cache and the cooldown state.
	DataDir string `yaml:"data_dir"`

	// Upstream endpoints. The model gateway and the asset/account gateway share
	// one host but different auth headers; the account service (login, SMS,
	// captcha, user profile) lives on the teamshare host.
	BaseURL        string `yaml:"base_url"`
	AccountBaseURL string `yaml:"account_base_url"`

	// Primary account straight from the config file. Both values are required
	// together: authorization is the JWT, organization_code the org it belongs
	// to (the asset API answers 2179 without it).
	Authorization      string `yaml:"authorization"`
	OrganizationCode   string `yaml:"organization_code"`
	DeviceID           string `yaml:"device_id"`
	DeviceName         string `yaml:"device_name"`
	CredentialAlias    string `yaml:"credential_alias"`
	CredentialPriority int    `yaml:"credential_priority"`

	// AccountsDir imports every *.json in a directory as one account.
	AccountsDir    string `yaml:"accounts_dir"`
	ImportInterval int    `yaml:"import_interval_seconds"`

	// ModelPrefix namespaces upstream ids, e.g. magic/glm-5.3-flash.
	ModelPrefix string `yaml:"model_prefix"`
	// ModelDenylist hides models from the catalogue by upstream id.
	ModelDenylist []string `yaml:"model_denylist"`
	// RefreshSeconds is the model catalogue refresh interval.
	RefreshSeconds int `yaml:"refresh_seconds"`

	// Local token bucket per account. Over-limit requests answer a local 429 so
	// the host can pick another credential instead of hammering one account.
	RPM   int `yaml:"rpm"`
	Burst int `yaml:"burst"`
	// CooldownSeconds parks an account after a retryable upstream failure.
	CooldownSeconds int `yaml:"cooldown_seconds"`
	// RequestTimeoutSeconds bounds a whole upstream call.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
	// MaxAttempts is how many accounts one request may try.
	MaxAttempts int `yaml:"max_attempts"`

	// PointsLogging attaches the point cost of each request to the console's
	// request log by matching the asset ledger after the fact.
	PointsLogging bool `yaml:"points_logging"`
	// DailyPointsEnabled claims the daily login gift and the manual onboarding
	// rewards for every account.
	DailyPointsEnabled bool `yaml:"daily_points_enabled"`

	// ProxyURL routes upstream calls through an HTTP proxy (empty = direct).
	ProxyURL string `yaml:"proxy_url"`
	// DebugDump logs payload bodies (truncated) into the console log.
	DebugDump bool `yaml:"debug_dump"`
}

const (
	defaultBaseURL        = "https://i-magic-service.letsmagic.cn"
	defaultAccountBaseURL = "https://i-keewood-service.teamshare.cn"
	defaultDataDir        = "./plugin-data/magic2api"
	defaultModelPrefix    = "magic"
)

func defaultConfig() Config {
	return Config{
		DataDir:               defaultDataDir,
		BaseURL:               defaultBaseURL,
		AccountBaseURL:        defaultAccountBaseURL,
		ModelPrefix:           defaultModelPrefix,
		ImportInterval:        30,
		RefreshSeconds:        1800,
		RPM:                   30,
		Burst:                 20,
		CooldownSeconds:       60,
		RequestTimeoutSeconds: 300,
		MaxAttempts:           3,
		PointsLogging:         true,
		DailyPointsEnabled:    true,
	}
}

func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("invalid plugin config: %w", err)
		}
	}
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	c.DataDir = strings.TrimSpace(c.DataDir)
	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}
	if abs, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = abs
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if u, err := url.Parse(c.BaseURL); err != nil || u.Host == "" {
		return fmt.Errorf("invalid base_url: %q", c.BaseURL)
	}
	c.AccountBaseURL = strings.TrimRight(strings.TrimSpace(c.AccountBaseURL), "/")
	if c.AccountBaseURL == "" {
		c.AccountBaseURL = defaultAccountBaseURL
	}
	if u, err := url.Parse(c.AccountBaseURL); err != nil || u.Host == "" {
		return fmt.Errorf("invalid account_base_url: %q", c.AccountBaseURL)
	}

	c.Authorization = strings.TrimSpace(c.Authorization)
	c.OrganizationCode = strings.TrimSpace(c.OrganizationCode)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.DeviceName = strings.TrimSpace(c.DeviceName)
	if (c.Authorization == "") != (c.OrganizationCode == "") {
		return fmt.Errorf("authorization 与 organization_code 必须同时提供")
	}

	c.CredentialAlias = sanitizeAlias(c.CredentialAlias)
	if c.CredentialAlias == "" {
		c.CredentialAlias = "default"
	}
	if c.CredentialPriority <= 0 {
		c.CredentialPriority = 10
	}

	c.AccountsDir = strings.TrimSpace(c.AccountsDir)
	c.ModelPrefix = strings.Trim(strings.TrimSpace(c.ModelPrefix), "/")
	if c.ModelPrefix == "" {
		c.ModelPrefix = defaultModelPrefix
	}
	if c.ImportInterval <= 0 {
		c.ImportInterval = 30
	}
	if c.RefreshSeconds <= 0 {
		c.RefreshSeconds = 1800
	}
	if c.RPM <= 0 {
		c.RPM = 30
	}
	if c.Burst <= 0 {
		c.Burst = 20
	}
	if c.CooldownSeconds < 0 {
		c.CooldownSeconds = 0
	}
	if c.RequestTimeoutSeconds <= 0 {
		c.RequestTimeoutSeconds = 300
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	c.ProxyURL = strings.TrimSpace(c.ProxyURL)
	return nil
}

func (c Config) requestTimeout() int { return c.RequestTimeoutSeconds }

func (c Config) importInterval() int { return c.ImportInterval }

func (c Config) modelPrefix() string { return c.ModelPrefix }

// qualifyModel adds the namespace prefix unless it is already there.
func (c Config) qualifyModel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, c.ModelPrefix+"/") {
		return id
	}
	return c.ModelPrefix + "/" + id
}

// upstreamModel strips the namespace prefix and reports whether the id belongs
// to this provider.
func (c Config) upstreamModel(id string) (string, bool) {
	id = strings.TrimSpace(id)
	prefix := c.ModelPrefix + "/"
	if strings.HasPrefix(id, prefix) {
		return strings.TrimPrefix(id, prefix), true
	}
	if !strings.Contains(id, "/") {
		return id, true
	}
	return "", false
}

func sanitizeAlias(alias string) string {
	alias = strings.TrimSpace(alias)
	var b strings.Builder
	for _, r := range alias {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func ensureDir(path string) error {
	if path == "" {
		return nil
	}
	return os.MkdirAll(path, 0o755)
}
