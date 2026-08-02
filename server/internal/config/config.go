// Package config loads pushfree server configuration.
//
// Configuration sources, in order of increasing precedence:
//  1. Built-in defaults (see defaults()).
//  2. A TOML file (BurntSushi/toml), if a path is provided.
//  3. PUSHFREE_* environment variables (env always wins).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// SchemaVersion is the config schema version this binary expects. A config
// whose resolved "version" differs from this value is rejected at startup so
// schema drift is caught loudly rather than silently mishandled.
const SchemaVersion = 1

// Config holds all server configuration. TOML keys use dashes; the matching
// PUSHFREE_* env vars use underscores in their place.
type Config struct {
	Version              int      `toml:"version"`
	ListenAddr           string   `toml:"listen-addr"`
	TLSCertFile          string   `toml:"tls-cert-file"`
	TLSKeyFile           string   `toml:"tls-key-file"`
	BaseURL              string   `toml:"base-url"`
	DBFile               string   `toml:"db-file"`
	DBURL                string   `toml:"db-url"`
	FCMCredentialsFile   string   `toml:"fcm-credentials-file"`
	KeepaliveInterval    string   `toml:"keepalive-interval"`
	QuotaMonthly         int      `toml:"quota-monthly"`
	MessagesRetention    string   `toml:"messages-retention"`
	AttachmentRetention  string   `toml:"attachment-retention"`
	SweeperInterval      string   `toml:"sweeper-interval"`
	ShutdownTimeout      string   `toml:"shutdown-timeout"`
	ShutdownOnStdinEOF   bool     `toml:"shutdown-on-stdin-eof"`
	CallbackAllowedHosts []string `toml:"callback-allowed-hosts"`
	AuthSecret           string   `toml:"auth-secret"`
}

// defaults returns a Config populated with the documented default values.
func defaults() Config {
	return Config{
		Version:            SchemaVersion,
		ListenAddr:         ":2586",
		TLSCertFile:        "",
		TLSKeyFile:         "",
		BaseURL:            "",
		DBFile:             "pushfree.db",
		DBURL:              "",
		FCMCredentialsFile: "",
		KeepaliveInterval:  "45s",
		QuotaMonthly:       10000,
		MessagesRetention:  "720h",
		// AttachmentRetention: Pushover drops undownloaded attachments
		// after 3 days; we match that. 30d message retention vs Pushover's
		// 21d is a documented deviation (see internal/store/sqlite/retention.go).
		AttachmentRetention:  "72h",
		SweeperInterval:      "1h",
		ShutdownTimeout:      "10s",
		ShutdownOnStdinEOF:   false,
		CallbackAllowedHosts: []string{},
	}
}

// LookupEnv returns the value of the named environment variable. os.LookupEnv
// satisfies this; tests pass a fake.
type LookupEnv func(key string) (string, bool)

// Load resolves configuration from defaults, then an optional TOML file at
// path, then PUSHFREE_* environment overrides (env wins). The result is
// validated; a non-nil error describes the first problem and names the
// offending field(s).
func Load(path string, env LookupEnv) (*Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	cfg := defaults()
	if path != "" {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return nil, fmt.Errorf("decode config %q: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, env); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnv overlays PUSHFREE_* values on top of the file/defaults. A malformed
// env value is an error (better to fail loudly than to silently keep a stale
// file value when the operator clearly tried to override it).
func applyEnv(cfg *Config, env LookupEnv) error {
	if v, ok := env("PUSHFREE_VERSION"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("PUSHFREE_VERSION %q: %w", v, err)
		}
		cfg.Version = n
	}
	if v, ok := env("PUSHFREE_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}
	if v, ok := env("PUSHFREE_TLS_CERT_FILE"); ok {
		cfg.TLSCertFile = v
	}
	if v, ok := env("PUSHFREE_TLS_KEY_FILE"); ok {
		cfg.TLSKeyFile = v
	}
	if v, ok := env("PUSHFREE_BASE_URL"); ok {
		cfg.BaseURL = v
	}
	if v, ok := env("PUSHFREE_DB_FILE"); ok {
		cfg.DBFile = v
	}
	if v, ok := env("PUSHFREE_DB_URL"); ok {
		cfg.DBURL = v
	}
	if v, ok := env("PUSHFREE_FCM_CREDENTIALS_FILE"); ok {
		cfg.FCMCredentialsFile = v
	}
	if v, ok := env("PUSHFREE_KEEPALIVE_INTERVAL"); ok {
		cfg.KeepaliveInterval = v
	}
	if v, ok := env("PUSHFREE_QUOTA_MONTHLY"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("PUSHFREE_QUOTA_MONTHLY %q: %w", v, err)
		}
		cfg.QuotaMonthly = n
	}
	if v, ok := env("PUSHFREE_MESSAGES_RETENTION"); ok {
		cfg.MessagesRetention = v
	}
	if v, ok := env("PUSHFREE_ATTACHMENT_RETENTION"); ok {
		cfg.AttachmentRetention = v
	}
	if v, ok := env("PUSHFREE_SWEEPER_INTERVAL"); ok {
		cfg.SweeperInterval = v
	}
	if v, ok := env("PUSHFREE_SHUTDOWN_TIMEOUT"); ok {
		cfg.ShutdownTimeout = v
	}
	if v, ok := env("PUSHFREE_SHUTDOWN_ON_STDIN_EOF"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("PUSHFREE_SHUTDOWN_ON_STDIN_EOF %q: %w", v, err)
		}
		cfg.ShutdownOnStdinEOF = b
	}
	if v, ok := env("PUSHFREE_CALLBACK_ALLOWED_HOSTS"); ok {
		parts := strings.Split(v, ",")
		hosts := make([]string, 0, len(parts))
		for _, h := range parts {
			if trimmed := strings.TrimSpace(h); trimmed != "" {
				hosts = append(hosts, trimmed)
			}
		}
		cfg.CallbackAllowedHosts = hosts
	}
	if v, ok := env("PUSHFREE_AUTH_SECRET"); ok {
		cfg.AuthSecret = v
	}
	return nil
}

// validate enforces invariants the loader cannot express structurally.
func (c *Config) validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("config \"version\" is %d, this binary requires %d", c.Version, SchemaVersion)
	}
	certSet := c.TLSCertFile != ""
	keySet := c.TLSKeyFile != ""
	if certSet != keySet {
		return fmt.Errorf("config \"tls-cert-file\" (%q) and \"tls-key-file\" (%q) must both be set or both be empty", c.TLSCertFile, c.TLSKeyFile)
	}
	return nil
}
