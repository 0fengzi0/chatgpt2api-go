package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCreatesSecretsAndVerifiesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
host: "127.0.0.1"
port: 18081
data-dir: "`+filepath.ToSlash(dir)+`"
remote-management:
  secret-key: ""
api-keys: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, generated, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 2 {
		t.Fatalf("expected management and api key generation, got %#v", generated)
	}
	management := ""
	apiKey := ""
	for _, item := range generated {
		if len(item) > len("management-key=") && item[:len("management-key=")] == "management-key=" {
			management = item[len("management-key="):]
		}
		if len(item) > len("api-key=") && item[:len("api-key=")] == "api-key=" {
			apiKey = item[len("api-key="):]
		}
	}
	if !VerifySecret(cfg.ManagementSecretKey(), management) {
		t.Fatal("generated management key did not verify")
	}
	if len(cfg.APIKeys) != 1 || !VerifyAPIKey(cfg.APIKeys[0], apiKey) {
		t.Fatal("generated API key did not verify")
	}
}

func TestAPIKeyScalarAndHash(t *testing.T) {
	item := APIKey{KeyHash: HashAPIKey("sk-test"), Enabled: true}
	if !VerifyAPIKey(item, "sk-test") {
		t.Fatal("hash did not verify")
	}
	if VerifyAPIKey(item, "wrong") {
		t.Fatal("wrong key verified")
	}
}

func TestAPIKeyMappingDefaultsEnabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
data-dir: "`+filepath.ToSlash(dir)+`"
api-keys:
  - name: client
    key: sk-client
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.APIKeys) != 1 || !cfg.APIKeys[0].Enabled || !VerifyAPIKey(cfg.APIKeys[0], "sk-client") {
		t.Fatalf("mapping key was not enabled: %#v", cfg.APIKeys)
	}
	if cfg.APIKeys[0].Key != "" || cfg.APIKeys[0].KeyHash == "" {
		t.Fatalf("plaintext API key was not replaced by a hash: %#v", cfg.APIKeys[0])
	}
}

func TestPlaintextManagementKeyIsHashedOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
data-dir: "`+filepath.ToSlash(dir)+`"
remote-management:
  secret-key: plain-secret
api-keys:
  - name: client
    key: sk-client
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !isBcryptHash(cfg.ManagementSecretKey()) {
		t.Fatalf("management key was not hashed: %s", cfg.ManagementSecretKey())
	}
	if !VerifySecret(cfg.ManagementSecretKey(), "plain-secret") {
		t.Fatal("hashed management key does not verify")
	}
}

func TestRelativePathsResolveFromConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
data-dir: "./runtime"
auth-dir: "./auth"
api-keys:
  - name: client
    key: sk-client
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.DataDirPath(), filepath.Join(configDir, "runtime"); got != want {
		t.Fatalf("data dir = %q, want %q", got, want)
	}
	if got, want := cfg.AuthDirPath(), filepath.Join(configDir, "auth"); got != want {
		t.Fatalf("auth dir = %q, want %q", got, want)
	}
}

func TestValidateProxyURL(t *testing.T) {
	for _, value := range []string{"", "http://127.0.0.1:10808", "https://proxy.example.com", "socks5://127.0.0.1:1080", "socks5h://127.0.0.1:1080"} {
		if err := ValidateProxyURL(value); err != nil {
			t.Fatalf("ValidateProxyURL(%q): %v", value, err)
		}
	}
	for _, value := range []string{"127.0.0.1:10808", "ftp://proxy.example.com", "http://"} {
		if err := ValidateProxyURL(value); err == nil {
			t.Fatalf("ValidateProxyURL(%q) unexpectedly succeeded", value)
		}
	}
}

func TestSentinelDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("data-dir: \""+filepath.ToSlash(dir)+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sentinel.Headless || !cfg.Sentinel.NoSandbox {
		t.Fatalf("unexpected sentinel browser defaults: %#v", cfg.Sentinel)
	}
	if cfg.Sentinel.Timezone != "Asia/Shanghai" {
		t.Fatalf("unexpected sentinel timezone: %q", cfg.Sentinel.Timezone)
	}
	if cfg.Sentinel.RefreshMinutes != 30 || cfg.Sentinel.NavigateTimeoutSecs != 60 || cfg.Sentinel.SolveTimeoutSeconds != 120 {
		t.Fatalf("unexpected sentinel timeout defaults: %#v", cfg.Sentinel)
	}
}

func TestImageGatewayDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Performance.ImageWorkers != 3 || cfg.Image.AccountConcurrency != 1 {
		t.Fatalf("unexpected concurrency defaults: performance=%d account=%d",
			cfg.Performance.ImageWorkers, cfg.Image.AccountConcurrency)
	}
	if cfg.Image.RequestTimeoutSeconds != 300 || cfg.Image.TemporaryURLTTLSeconds != 600 {
		t.Fatalf("unexpected timeout defaults: request=%d url=%d",
			cfg.Image.RequestTimeoutSeconds, cfg.Image.TemporaryURLTTLSeconds)
	}
	if cfg.Image.LogRetentionDays != 30 || cfg.Image.MaxImagePixels != 16*1024*1024 {
		t.Fatalf("unexpected retention or pixel default: retention=%d pixels=%d",
			cfg.Image.LogRetentionDays, cfg.Image.MaxImagePixels)
	}
}

func TestLoadUpgradesUnsupportedImagePixelLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := "image:\n  max-image-pixels: 4194304\n  max-dimension: 4096\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image.MaxImagePixels != 16*1024*1024 {
		t.Fatalf("image pixel limit was not upgraded: %d", cfg.Image.MaxImagePixels)
	}
	if cfg.Image.MaxDimension != 4096 {
		t.Fatalf("supported image dimension limit changed: %d", cfg.Image.MaxDimension)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "max-image-pixels: 16777216") {
		t.Fatalf("upgraded image limit was not persisted: %s", data)
	}
}

func TestNormalizeUpgradesUnsupportedImageDimension(t *testing.T) {
	cfg := Default()
	cfg.Image.MaxImagePixels = MinimumSupportedImagePixels
	cfg.Image.MaxDimension = 2048
	cfg.Normalize()
	if cfg.Image.MaxImagePixels != MinimumSupportedImagePixels {
		t.Fatalf("supported image pixel limit changed: %d", cfg.Image.MaxImagePixels)
	}
	if cfg.Image.MaxDimension != 4096 {
		t.Fatalf("image dimension limit was not upgraded: %d", cfg.Image.MaxDimension)
	}
}

func TestNormalizeKeepsSupportedImageLimits(t *testing.T) {
	cfg := Default()
	cfg.Image.MaxImagePixels = MinimumSupportedImagePixels
	cfg.Image.MaxDimension = MinimumSupportedImageDimension
	cfg.Normalize()
	if cfg.Image.MaxImagePixels != MinimumSupportedImagePixels {
		t.Fatalf("supported image pixel limit changed: %d", cfg.Image.MaxImagePixels)
	}
	if cfg.Image.MaxDimension != MinimumSupportedImageDimension {
		t.Fatalf("supported image dimension limit changed: %d", cfg.Image.MaxDimension)
	}
}

func TestCORSAllowedOrigins(t *testing.T) {
	cfg := Default()
	for _, origin := range []string{
		"https://5ixf.com",
		"https://www.5ixf.com",
		"https://app.5ixf.com",
		"https://isancha.com",
		"https://api.ai.isancha.com",
	} {
		if !cfg.AllowsOrigin(origin) {
			t.Fatalf("origin should be allowed: %s", origin)
		}
	}
	for _, origin := range []string{
		"http://5ixf.com",
		"https://evil5ixf.com",
		"https://5ixf.com.evil.example",
		"https://example.com",
	} {
		if cfg.AllowsOrigin(origin) {
			t.Fatalf("origin should be rejected: %s", origin)
		}
	}
}
