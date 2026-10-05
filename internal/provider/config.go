package provider

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultGitHubClientID = "Iv1.b507a08c87ecfe98"
	defaultGitHubBaseURL  = "https://github.com"
	defaultGitHubAPIURL   = "https://api.github.com"
	defaultCopilotAPIURL  = "https://api.githubcopilot.com"
)

type ModelConfig struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
}

type Config struct {
	ModelPrefix              string        `yaml:"model_prefix"`
	Models                   []ModelConfig `yaml:"models"`
	Enabled                  bool          `yaml:"enabled"`
	GitHubClientID           string        `yaml:"github_client_id"`
	GitHubScope              string        `yaml:"github_scope"`
	GitHubBaseURL            string        `yaml:"github_base_url"`
	GitHubAPIURL             string        `yaml:"github_api_url"`
	CopilotAPIURL            string        `yaml:"copilot_api_url"`
	AllowCustomEndpoints     bool          `yaml:"allow_custom_endpoints"`
	AllowCustomScopes        bool          `yaml:"allow_custom_scopes"`
	AllowInsecureBaseURLs    bool          `yaml:"allow_insecure_base_urls"`
	OAuthTimeoutSeconds      int           `yaml:"oauth_timeout_seconds"`
	ModelCacheTTLSeconds     int           `yaml:"model_cache_ttl_seconds"`
	TokenExpiryBufferSeconds int           `yaml:"token_expiry_buffer_seconds"`
	ModelsExcluded           []string      `yaml:"models_excluded"`
	ModelPickerRequired      bool          `yaml:"model_picker_required"`
	AllowRawModelNames       bool          `yaml:"allow_raw_model_names"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:                  true,
		ModelPrefix:              "copilot",
		GitHubClientID:           DefaultGitHubClientID,
		GitHubScope:              "read:user",
		GitHubBaseURL:            defaultGitHubBaseURL,
		GitHubAPIURL:             defaultGitHubAPIURL,
		CopilotAPIURL:            defaultCopilotAPIURL,
		OAuthTimeoutSeconds:      900,
		ModelCacheTTLSeconds:     60,
		TokenExpiryBufferSeconds: 300,
	}
}

func ParseConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	var supplied map[string]any
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &supplied); err != nil {
			return Config{}, fmt.Errorf("decode plugin config: %w", err)
		}
		if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
			return Config{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	cfg.ModelPrefix = strings.TrimRight(strings.TrimSpace(cfg.ModelPrefix), "/")
	if cfg.ModelPrefix == "" {
		cfg.ModelPrefix = "copilot"
	}
	seen := make(map[string]bool, len(cfg.Models))
	for i := range cfg.Models {
		model := &cfg.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" {
			return Config{}, fmt.Errorf("each configured model requires a name")
		}
		if model.Alias == "" {
			model.Alias = cfg.exposedModelID(model.Name)
		}
		if seen[model.Alias] {
			return Config{}, fmt.Errorf("model aliases must be unique")
		}
		seen[model.Alias] = true
	}
	cfg.GitHubClientID = strings.TrimSpace(cfg.GitHubClientID)
	cfg.GitHubScope = strings.Join(strings.Fields(cfg.GitHubScope), " ")
	if !cfg.AllowCustomScopes && cfg.GitHubScope != "" && cfg.GitHubScope != "read:user" {
		return Config{}, fmt.Errorf("github_scope beyond read:user requires allow_custom_scopes: true")
	}
	cfg.GitHubBaseURL = strings.TrimRight(strings.TrimSpace(cfg.GitHubBaseURL), "/")
	cfg.GitHubAPIURL = strings.TrimRight(strings.TrimSpace(cfg.GitHubAPIURL), "/")
	cfg.CopilotAPIURL = strings.TrimRight(strings.TrimSpace(cfg.CopilotAPIURL), "/")
	cfg.ModelsExcluded = normalizeModelPrefixes(cfg.ModelsExcluded)
	if err := cfg.configureEnterprise(supplied); err != nil {
		return Config{}, err
	}
	if cfg.GitHubClientID == "" {
		return Config{}, fmt.Errorf("github_client_id is required")
	}

	for name, value := range map[string]string{
		"github_base_url": cfg.GitHubBaseURL,
		"github_api_url":  cfg.GitHubAPIURL,
		"copilot_api_url": cfg.CopilotAPIURL,
	} {
		if errURL := validateBaseURL(value, cfg.AllowInsecureBaseURLs); errURL != nil {
			return Config{}, fmt.Errorf("%s: %w", name, errURL)
		}
	}
	if err := cfg.validateEndpointTrust(); err != nil {
		return Config{}, err
	}
	if cfg.OAuthTimeoutSeconds < 60 || cfg.OAuthTimeoutSeconds > 1800 {
		return Config{}, fmt.Errorf("oauth_timeout_seconds must be between 60 and 1800")
	}
	if cfg.ModelCacheTTLSeconds < 30 || cfg.ModelCacheTTLSeconds > 60 {
		return Config{}, fmt.Errorf("model_cache_ttl_seconds must be between 30 and 60")
	}
	if cfg.TokenExpiryBufferSeconds < 30 || cfg.TokenExpiryBufferSeconds > 900 {
		return Config{}, fmt.Errorf("token_expiry_buffer_seconds must be between 30 and 900")
	}
	return cfg, nil
}

func normalizeModelPrefixes(prefixes []string) []string {
	seen := make(map[string]struct{}, len(prefixes))
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		out = append(out, prefix)
	}
	return out
}

func validateBaseURL(raw string, allowInsecure bool) error {
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid absolute URL")
	}
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1")) {
		return fmt.Errorf("URL must use HTTPS")
	}
	if strings.HasSuffix(parsed.Hostname(), ".") {
		return fmt.Errorf("base URL hostname must not have a trailing dot")
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("base URL must not contain query or fragment")
	}
	return nil
}

func (c Config) oauthTimeout() time.Duration {
	return time.Duration(c.OAuthTimeoutSeconds) * time.Second
}

func (c Config) modelCacheTTL() time.Duration {
	return time.Duration(c.ModelCacheTTLSeconds) * time.Second
}

func (c Config) tokenExpiryBuffer() time.Duration {
	return time.Duration(c.TokenExpiryBufferSeconds) * time.Second
}

// enterpriseHost accepts a single tenant label, not arbitrary ghe.com subdomains.
func (c Config) enterpriseHost() string {
	u, err := url.Parse(c.GitHubBaseURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	label, ok := strings.CutSuffix(host, ".ghe.com")
	if !ok || label == "" || strings.Contains(label, ".") {
		return ""
	}
	for i, r := range label {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(label)-1) {
			return ""
		}
	}
	if len(label) > 63 {
		return ""
	}
	return host
}

func (c *Config) configureEnterprise(supplied map[string]any) error {
	base, err := url.Parse(c.GitHubBaseURL)
	if err != nil {
		return fmt.Errorf("invalid GitHub base URL")
	}
	host := c.enterpriseHost()
	if host == "" {
		// A partial tenant override must never keep public OAuth defaults.
		for _, raw := range []string{c.GitHubBaseURL, c.GitHubAPIURL, c.CopilotAPIURL} {
			u, e := url.Parse(raw)
			if e == nil && (strings.EqualFold(u.Hostname(), "ghe.com") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".ghe.com")) {
				return fmt.Errorf("GHE.com requires github_base_url https://TENANT.ghe.com")
			}
		}
		return nil
	}
	if base.Scheme != "https" || base.Port() != "" || base.Path != "" || base.User != nil || base.ForceQuery || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("GHE.com github_base_url must be a tenant HTTPS origin without a port or path")
	}
	c.GitHubBaseURL = "https://" + host
	if _, ok := supplied["github_api_url"]; !ok {
		c.GitHubAPIURL = "https://api." + host
	}
	if _, ok := supplied["copilot_api_url"]; !ok {
		c.CopilotAPIURL = "https://copilot-api." + host
	}
	if client, ok := supplied["github_client_id"].(string); !ok || strings.TrimSpace(client) == "" || strings.TrimSpace(c.GitHubClientID) == "" {
		return fmt.Errorf("GHE.com requires an explicit github_client_id approved for the tenant's device flow")
	}
	for name, raw := range map[string]string{"github_api_url": c.GitHubAPIURL, "copilot_api_url": c.CopilotAPIURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Port() != "" || u.Path != "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), "."+host) {
			return fmt.Errorf("%s must be an HTTPS origin within the configured GHE.com tenant", name)
		}
	}
	return nil
}

// SameAuthRouting reports whether a reconfiguration preserves all authentication
// destinations and trust settings. The plugin lifecycle checks this before
// replacing a service, including across a disable/re-enable cycle.
func (c Config) SameAuthRouting(other Config) bool {
	return c.GitHubBaseURL == other.GitHubBaseURL && c.GitHubAPIURL == other.GitHubAPIURL && c.CopilotAPIURL == other.CopilotAPIURL && c.GitHubClientID == other.GitHubClientID && c.GitHubScope == other.GitHubScope && c.AllowInsecureBaseURLs == other.AllowInsecureBaseURLs && c.AllowCustomEndpoints == other.AllowCustomEndpoints && c.AllowCustomScopes == other.AllowCustomScopes
}

func (c Config) validateEndpointTrust() error {
	// Enterprise configuration has its own stronger same-tenant restrictions;
	// neither custom-endpoint nor loopback flags can relax those restrictions.
	if c.enterpriseHost() != "" || c.AllowCustomEndpoints {
		return nil
	}
	for name, raw := range map[string]string{"github_base_url": c.GitHubBaseURL, "github_api_url": c.GitHubAPIURL, "copilot_api_url": c.CopilotAPIURL} {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s: invalid URL", name)
		}
		host := strings.ToLower(u.Hostname())
		if c.AllowInsecureBaseURLs && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
			continue
		}
		allowed := false
		if u.Scheme == "https" && u.Port() == "" && u.Path == "" && u.RawPath == "" {
			switch name {
			case "github_base_url":
				allowed = host == "github.com"
			case "github_api_url":
				allowed = host == "api.github.com"
			case "copilot_api_url":
				allowed = host == "api.githubcopilot.com" || strings.HasSuffix(host, ".githubcopilot.com")
			}
		}
		if !allowed {
			return fmt.Errorf("%s uses a custom endpoint; explicitly enable allow_custom_endpoints to trust it", name)
		}
	}
	return nil
}
