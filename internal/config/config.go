package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"

	"github.com/sirupsen/logrus"
	json5 "github.com/yosuke-furukawa/json5/encoding/json5"
)

// UserAgent is the HTTP User-Agent used for upstream requests.
// It can be overridden at runtime (e.g., from config).
var UserAgent = "google-api-nodejs-client/9.15.1"

type Config struct {
	Host                 string   `json:"host"`
	ServerPort           int      `json:"port"`
	AuthKey              string   `json:"authKey"`
	GeminiCredsFilePaths []string `json:"geminiOauthCredsFiles"`
	// Optional user agent for upstream requests; if empty, a default is used.
	UserAgent string `json:"userAgent"`
	// ProjectIds maps a credential path to an ordered list of project IDs.
	// Keys must match one of the entries in geminiOauthCredsFiles after ~ expansion.
	// If a key exists with an empty list, it is treated as not configured and
	// discovery will be used for that credential.
	ProjectIds             map[string][]string `json:"projectIds"`
	RequestMaxRetries      int                 `json:"requestMaxRetries"`
	RequestBaseDelayMillis int                 `json:"requestBaseDelay"`
	SQLitePath             string              `json:"sqlitePath"`
	// Proxy is an optional upstream proxy URL. Must be http or socks5.
	// Example: "http://127.0.0.1:8080" or "socks5://127.0.0.1:1080"
	Proxy string `json:"proxy"`
	// RequestMaxBodyBytes limits incoming request size to mitigate DoS via large payloads.
	// If zero, a safe default is applied.
	RequestMaxBodyBytes int64 `json:"requestMaxBodyBytes"`
	// MaxConcurrentRequests limits concurrent in-flight requests for lightweight backpressure.
	// If zero, a default value is applied.
	MaxConcurrentRequests int `json:"maxConcurrentRequests"`
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	logrus.Infof("loading config from %s", path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	// First pass: decode to a generic map to detect unknown keys (JSON5 allows comments etc.).
	var raw map[string]any
	if err := json5.NewDecoder(bytes.NewReader(b)).Decode(&raw); err != nil {
		// Surface syntax errors as parse errors
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	// Allowed top-level keys derived from Config struct tags
	allowed := map[string]struct{}{}
	allowedLower := map[string]struct{}{}
	ct := reflect.TypeOf(cfg)
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			// Still allow matching by field name
			name := f.Name
			allowed[name] = struct{}{}
			allowedLower[strings.ToLower(name)] = struct{}{}
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		allowed[name] = struct{}{}
		allowedLower[strings.ToLower(name)] = struct{}{}
		// Also allow using the exported field name directly (case-insensitive)
		allowed[f.Name] = struct{}{}
		allowedLower[strings.ToLower(f.Name)] = struct{}{}
	}
	for k := range raw {
		if _, ok := allowed[k]; ok {
			continue
		}
		if _, ok := allowedLower[strings.ToLower(k)]; !ok {
			return cfg, fmt.Errorf("unknown config key: %s", k)
		}
	}
	// Second pass: decode into the strongly-typed struct.
	if err := json5.NewDecoder(bytes.NewReader(b)).Decode(&cfg); err != nil {
		// Keep error semantics consistent
		var se *json5.SyntaxError
		if !errors.As(err, &se) {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	// Log user agent if provided
	if strings.TrimSpace(cfg.UserAgent) != "" {
		logrus.Infof("using user agent: %s", cfg.UserAgent)
		UserAgent = cfg.UserAgent
	}
	// Defaults
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.ServerPort == 0 {
		cfg.ServerPort = 8085
	}
	if cfg.RequestMaxRetries == 0 {
		cfg.RequestMaxRetries = 3
	}
	if cfg.RequestBaseDelayMillis == 0 {
		cfg.RequestBaseDelayMillis = 1000
	}
	if cfg.SQLitePath == "" {
		cfg.SQLitePath = "./data/state.db"
	}
	if cfg.RequestMaxBodyBytes == 0 {
		// 16 MiB by default
		cfg.RequestMaxBodyBytes = 16 * 1024 * 1024
	}
	if cfg.MaxConcurrentRequests == 0 {
		cfg.MaxConcurrentRequests = 64
	}
	return cfg, nil
}

func (c Config) Validate(cfgPath string) error {
	if c.AuthKey == "" {
		return fmt.Errorf("authKey must be set in config file %s", cfgPath)
	}
	// Fail when authKey equals the default placeholder from example file.
	if c.AuthKey == "UNSAFE-KEY-REPLACE" {
		return fmt.Errorf("authKey must be changed from default placeholder")
	}
	// Validate proxy scheme if provided
	if c.Proxy != "" {
		u, err := url.Parse(c.Proxy)
		if err != nil {
			return fmt.Errorf("invalid proxy URL: %w", err)
		}
		switch u.Scheme {
		case "http", "socks5":
			// ok
		default:
			return fmt.Errorf("proxy scheme must be http or socks5")
		}
		if u.Host == "" {
			return fmt.Errorf("proxy URL must include host:port")
		}
	}
	// Validate that projectIds keys (after ~ expansion) match one of the
	// configured credential paths (also after ~ expansion). Do not resolve symlinks.
	if len(c.ProjectIds) > 0 {
		// Build set of expanded credential paths
		expanded := make(map[string]struct{}, len(c.GeminiCredsFilePaths))
		for _, p := range c.GeminiCredsFilePaths {
			if p == "" {
				continue
			}
			xp, err := expandUser(p)
			if err != nil {
				return fmt.Errorf("expand creds path %q: %w", p, err)
			}
			expanded[xp] = struct{}{}
		}
		// Validate each projectIds key
		for k := range c.ProjectIds {
			xp, err := expandUser(k)
			if err != nil {
				return fmt.Errorf("expand projectIds key %q: %w", k, err)
			}
			if _, ok := expanded[xp]; !ok {
				return fmt.Errorf("projectIds key %q does not match any geminiOauthCredsFiles entry", k)
			}
		}
	}
	return nil
}

// expandUser mirrors internal/utils.ExpandUser without importing utils to avoid cycles.
func expandUser(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return home + string(os.PathSeparator) + strings.TrimPrefix(path, "~/"), nil
	}
	return path, nil
}
