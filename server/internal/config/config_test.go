package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func noEnv(string) (string, bool) { return "", false }

func TestLoadDefaultsNoFile(t *testing.T) {
	cfg, err := Load("", noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", cfg.Version, SchemaVersion)
	}
	if cfg.ListenAddr != ":2586" {
		t.Errorf("default listen-addr = %q, want :2586", cfg.ListenAddr)
	}
	if cfg.DBFile != "pushfree.db" {
		t.Errorf("default db-file = %q, want pushfree.db", cfg.DBFile)
	}
	if cfg.KeepaliveInterval != "45s" {
		t.Errorf("default keepalive-interval = %q, want 45s", cfg.KeepaliveInterval)
	}
	if cfg.QuotaMonthly != 10000 {
		t.Errorf("default quota-monthly = %d, want 10000", cfg.QuotaMonthly)
	}
	if cfg.MessagesRetention != "720h" {
		t.Errorf("default messages-retention = %q, want 720h", cfg.MessagesRetention)
	}
	if len(cfg.CallbackAllowedHosts) != 0 {
		t.Errorf("default callback-allowed-hosts = %v, want empty", cfg.CallbackAllowedHosts)
	}
}

func TestLoadTOML(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `
version = 1
listen-addr = ":7777"
quota-monthly = 42
base-url = "https://push.example.com"
callback-allowed-hosts = ["hooks.example.com", "monitor.local"]
`)
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("listen-addr = %q, want :7777", cfg.ListenAddr)
	}
	if cfg.QuotaMonthly != 42 {
		t.Errorf("quota-monthly = %d, want 42", cfg.QuotaMonthly)
	}
	if cfg.BaseURL != "https://push.example.com" {
		t.Errorf("base-url = %q", cfg.BaseURL)
	}
	if cfg.DBFile != "pushfree.db" {
		t.Errorf("db-file should keep default = %q, want pushfree.db", cfg.DBFile)
	}
	if len(cfg.CallbackAllowedHosts) != 2 || cfg.CallbackAllowedHosts[0] != "hooks.example.com" {
		t.Errorf("callback-allowed-hosts = %v", cfg.CallbackAllowedHosts)
	}
}

func TestEnvOverlayPrecedence(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `listen-addr = ":7777"`)
	cfg, err := Load(p, func(key string) (string, bool) {
		if key == "PUSHFREE_LISTEN_ADDR" {
			return ":9999", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("env must override file: listen-addr = %q, want :9999", cfg.ListenAddr)
	}
}

func TestEnvOverlayIntAndList(t *testing.T) {
	cfg, err := Load("", func(key string) (string, bool) {
		switch key {
		case "PUSHFREE_QUOTA_MONTHLY":
			return "5", true
		case "PUSHFREE_CALLBACK_ALLOWED_HOSTS":
			return "a.example.com,b.example.com", true
		case "PUSHFREE_DB_URL":
			return "postgres://reserved", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QuotaMonthly != 5 {
		t.Errorf("quota-monthly = %d, want 5", cfg.QuotaMonthly)
	}
	if cfg.DBURL != "postgres://reserved" {
		t.Errorf("db-url = %q", cfg.DBURL)
	}
	if len(cfg.CallbackAllowedHosts) != 2 || cfg.CallbackAllowedHosts[1] != "b.example.com" {
		t.Errorf("callback-allowed-hosts = %v", cfg.CallbackAllowedHosts)
	}
}

func TestVersionMismatch(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `version = 999`)
	_, err := Load(p, noEnv)
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error must name field 'version': %v", err)
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "1") {
		t.Errorf("error must state got and want versions: %v", err)
	}
}

func TestVersionMismatchViaEnv(t *testing.T) {
	_, err := Load("", func(key string) (string, bool) {
		if key == "PUSHFREE_VERSION" {
			return "2", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected version mismatch error from env, got nil")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error must name field 'version': %v", err)
	}
}

func TestTLSHalfConfiguredCertOnly(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `version = 1
tls-cert-file = "cert.pem"`)
	_, err := Load(p, noEnv)
	if err == nil {
		t.Fatal("expected TLS half-config error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tls-cert-file") || !strings.Contains(msg, "tls-key-file") {
		t.Errorf("error must name both tls fields: %v", err)
	}
}

func TestTLSHalfConfiguredKeyOnly(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `version = 1
tls-key-file = "key.pem"`)
	_, err := Load(p, noEnv)
	if err == nil {
		t.Fatal("expected TLS half-config error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tls-cert-file") || !strings.Contains(msg, "tls-key-file") {
		t.Errorf("error must name both tls fields: %v", err)
	}
}

func TestTLSBothConfiguredOK(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `version = 1
tls-cert-file = "cert.pem"
tls-key-file = "key.pem"`)
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatalf("both set must be valid: %v", err)
	}
	if cfg.TLSCertFile != "cert.pem" || cfg.TLSKeyFile != "key.pem" {
		t.Errorf("tls fields = %q / %q", cfg.TLSCertFile, cfg.TLSKeyFile)
	}
}

func TestMalformedTOML(t *testing.T) {
	p := writeTemp(t, "pushfree.toml", `listen-addr = ":7777" = = broken`)
	_, err := Load(p, noEnv)
	if err == nil {
		t.Fatal("expected malformed TOML error, got nil")
	}
}
