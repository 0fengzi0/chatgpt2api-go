package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigPath = "config.yaml"
	DefaultDataDir    = "./data"
)

type APIKey struct {
	Name      string `yaml:"name" json:"name"`
	Key       string `yaml:"key,omitempty" json:"key,omitempty"`
	KeyHash   string `yaml:"key-hash,omitempty" json:"key_hash,omitempty"`
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	ExpiresAt string `yaml:"expires-at,omitempty" json:"expires_at,omitempty"`
	CreatedAt string `yaml:"created-at,omitempty" json:"created_at,omitempty"`
}

func (k *APIKey) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		k.Name = "default"
		k.Key = strings.TrimSpace(value.Value)
		k.Enabled = true
		return nil
	}
	type plain APIKey
	v := plain{Enabled: true}
	if err := value.Decode(&v); err != nil {
		return err
	}
	*k = APIKey(v)
	return nil
}

func (k *APIKey) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err == nil {
		if s, ok := raw.(string); ok {
			k.Name = "default"
			k.Key = strings.TrimSpace(s)
			k.Enabled = true
			return nil
		}
	}
	type plain APIKey
	v := plain{Enabled: true}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*k = APIKey(v)
	return nil
}

type RemoteManagement struct {
	SecretKey string `yaml:"secret-key" json:"secret_key"`
}

type ImageConfig struct {
	AccountConcurrency        int   `yaml:"account-concurrency" json:"account_concurrency"`
	PollTimeoutSeconds        int   `yaml:"poll-timeout-seconds" json:"poll_timeout_seconds"`
	PollIntervalSeconds       int   `yaml:"poll-interval-seconds" json:"poll_interval_seconds"`
	RequestTimeoutSeconds     int   `yaml:"request-timeout-seconds" json:"request_timeout_seconds"`
	TemporaryURLTTLSeconds    int   `yaml:"temporary-url-ttl-seconds" json:"temporary_url_ttl_seconds"`
	QueueSize                 int   `yaml:"queue-size" json:"queue_size"`
	MaxUploadBytes            int64 `yaml:"max-upload-bytes" json:"max_upload_bytes"`
	MaxImageBytes             int64 `yaml:"max-image-bytes" json:"max_image_bytes"`
	MaxImagePixels            int64 `yaml:"max-image-pixels" json:"max_image_pixels"`
	MaxDimension              int   `yaml:"max-dimension" json:"max_dimension"`
	LogRetentionDays          int   `yaml:"log-retention-days" json:"log_retention_days"`
	AutoRemoveInvalidAccounts bool  `yaml:"auto-remove-invalid-accounts" json:"auto_remove_invalid_accounts"`
}

type PerformanceConfig struct {
	ImageWorkers          int    `yaml:"image-workers" json:"image_workers"`
	ParallelGeneration    bool   `yaml:"parallel-generation" json:"parallel_generation"`
	AccountRefreshWorkers int    `yaml:"account-refresh-workers" json:"account_refresh_workers"`
	TextCacheEntries      int    `yaml:"text-cache-entries" json:"text_cache_entries"`
	MetricsMode           string `yaml:"metrics-mode" json:"metrics_mode"`
}

type QuotaExceededConfig struct {
	SwitchAccount bool `yaml:"switch-account" json:"switch_account"`
	SwitchModel   bool `yaml:"switch-model" json:"switch_model"`
}

type SentinelConfig struct {
	BrowserPath         string `yaml:"browser-path" json:"browser_path"`
	Timezone            string `yaml:"timezone" json:"timezone"`
	Headless            bool   `yaml:"headless" json:"headless"`
	NoSandbox           bool   `yaml:"no-sandbox" json:"no_sandbox"`
	RefreshMinutes      int    `yaml:"refresh-minutes" json:"refresh_minutes"`
	NavigateTimeoutSecs int    `yaml:"navigate-timeout-seconds" json:"navigate_timeout_seconds"`
	SolveTimeoutSeconds int    `yaml:"solve-timeout-seconds" json:"solve_timeout_seconds"`
}

type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed-origins" json:"allowed_origins"`
}

type Config struct {
	Host                  string              `yaml:"host" json:"host"`
	Port                  int                 `yaml:"port" json:"port"`
	RemoteManagement      RemoteManagement    `yaml:"remote-management" json:"remote_management"`
	APIKeys               []APIKey            `yaml:"api-keys" json:"api_keys"`
	AuthDir               string              `yaml:"auth-dir" json:"auth_dir"`
	DataDir               string              `yaml:"data-dir" json:"data_dir"`
	Debug                 bool                `yaml:"debug" json:"debug"`
	UsageStatistics       bool                `yaml:"usage-statistics-enabled" json:"usage_statistics_enabled"`
	RefreshIntervalMinute int                 `yaml:"refresh-interval-minutes" json:"refresh_interval_minutes"`
	LoggingToFile         bool                `yaml:"logging-to-file" json:"logging_to_file"`
	ProxyURL              string              `yaml:"proxy-url" json:"proxy_url"`
	Image                 ImageConfig         `yaml:"image" json:"image"`
	Performance           PerformanceConfig   `yaml:"performance" json:"performance"`
	QuotaExceeded         QuotaExceededConfig `yaml:"quota-exceeded" json:"quota_exceeded"`
	OpenAICompatibility   []map[string]any    `yaml:"openai-compatibility" json:"openai_compatibility"`
	ProviderBaseURL       string              `yaml:"provider-base-url" json:"provider_base_url"`
	Sentinel              SentinelConfig      `yaml:"sentinel" json:"sentinel"`
	CORS                  CORSConfig          `yaml:"cors" json:"cors"`
	PublicBaseURL         string              `yaml:"public-base-url" json:"public_base_url"`
	SigningKey            string              `yaml:"signing-key" json:"signing_key"`
	SensitiveWords        []string            `yaml:"sensitive-words" json:"sensitive_words"`
	MaxRequestBodyBytes   int64               `yaml:"max-request-body-bytes" json:"max_request_body_bytes"`
	ImageGenerationModel  string              `yaml:"image-generation-model" json:"image_generation_model"`
	DefaultUpstreamModel  string              `yaml:"default-upstream-model" json:"default_upstream_model"`
	DefaultThinkingEffort string              `yaml:"default-thinking-effort" json:"default_thinking_effort"`
	baseDir               string
}

func Default() Config {
	return Config{
		Host:                  "127.0.0.1",
		Port:                  3000,
		AuthDir:               "./data/auth",
		DataDir:               DefaultDataDir,
		UsageStatistics:       true,
		RefreshIntervalMinute: 30,
		ProviderBaseURL:       "https://chatgpt.com",
		Image: ImageConfig{
			AccountConcurrency:     1,
			PollTimeoutSeconds:     120,
			PollIntervalSeconds:    10,
			RequestTimeoutSeconds:  300,
			TemporaryURLTTLSeconds: 600,
			QueueSize:              100,
			MaxUploadBytes:         25 * 1024 * 1024,
			MaxImageBytes:          20 * 1024 * 1024,
			MaxImagePixels:         16 * 1024 * 1024,
			MaxDimension:           4096,
			LogRetentionDays:       30,
		},
		Performance: PerformanceConfig{
			ImageWorkers:          3,
			ParallelGeneration:    false,
			AccountRefreshWorkers: 1,
			TextCacheEntries:      0,
			MetricsMode:           "light",
		},
		QuotaExceeded: QuotaExceededConfig{SwitchAccount: true},
		Sentinel: SentinelConfig{
			Timezone:            "Asia/Shanghai",
			Headless:            true,
			NoSandbox:           true,
			RefreshMinutes:      30,
			NavigateTimeoutSecs: 60,
			SolveTimeoutSeconds: 120,
		},
		CORS: CORSConfig{AllowedOrigins: []string{
			"https://5ixf.com",
			"https://www.5ixf.com",
			"https://*.5ixf.com",
			"https://isancha.com",
			"https://www.isancha.com",
			"https://*.isancha.com",
		}},
		MaxRequestBodyBytes:   105 * 1024 * 1024,
		ImageGenerationModel:  "gpt-image-2",
		DefaultUpstreamModel:  "gpt-5-5",
		DefaultThinkingEffort: "auto",
	}
}

func (c *Config) Normalize() {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = 3000
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.AuthDir == "" {
		c.AuthDir = filepath.Join(c.DataDir, "auth")
	}
	if c.RefreshIntervalMinute < 1 {
		c.RefreshIntervalMinute = 30
	}
	if c.Image.AccountConcurrency < 1 {
		c.Image.AccountConcurrency = 1
	}
	if c.Image.PollTimeoutSeconds < 5 {
		c.Image.PollTimeoutSeconds = 120
	}
	if c.Image.PollIntervalSeconds < 1 {
		c.Image.PollIntervalSeconds = 10
	}
	if c.Image.RequestTimeoutSeconds < 30 {
		c.Image.RequestTimeoutSeconds = 300
	}
	if c.Image.TemporaryURLTTLSeconds < 60 {
		c.Image.TemporaryURLTTLSeconds = 600
	}
	if c.Image.QueueSize < 1 {
		c.Image.QueueSize = 100
	}
	if c.Image.MaxUploadBytes < 1 {
		c.Image.MaxUploadBytes = 25 * 1024 * 1024
	}
	if c.Image.MaxImageBytes < 1 {
		c.Image.MaxImageBytes = 20 * 1024 * 1024
	}
	if c.Image.MaxImagePixels < 1 {
		c.Image.MaxImagePixels = 16 * 1024 * 1024
	}
	if c.Image.MaxDimension < 256 {
		c.Image.MaxDimension = 4096
	}
	if c.Image.LogRetentionDays < 1 {
		c.Image.LogRetentionDays = 30
	}
	if c.Performance.ImageWorkers < 1 {
		c.Performance.ImageWorkers = 3
	}
	if c.Performance.ImageWorkers > 64 {
		c.Performance.ImageWorkers = 64
	}
	if c.Performance.AccountRefreshWorkers < 1 {
		c.Performance.AccountRefreshWorkers = 1
	}
	c.ProxyURL = strings.TrimSpace(c.ProxyURL)
	words := c.SensitiveWords[:0]
	for _, word := range c.SensitiveWords {
		if word = strings.TrimSpace(word); word != "" {
			words = append(words, word)
		}
	}
	c.SensitiveWords = words
	c.Sentinel.BrowserPath = strings.TrimSpace(c.Sentinel.BrowserPath)
	c.Sentinel.Timezone = strings.TrimSpace(c.Sentinel.Timezone)
	if c.Sentinel.Timezone == "" {
		c.Sentinel.Timezone = "Asia/Shanghai"
	}
	if c.Sentinel.RefreshMinutes < 1 {
		c.Sentinel.RefreshMinutes = 30
	}
	if c.Sentinel.NavigateTimeoutSecs < 5 {
		c.Sentinel.NavigateTimeoutSecs = 60
	}
	if c.Sentinel.SolveTimeoutSeconds < 5 {
		c.Sentinel.SolveTimeoutSeconds = 120
	}
	origins := c.CORS.AllowedOrigins[:0]
	for _, origin := range c.CORS.AllowedOrigins {
		if origin = strings.TrimSuffix(strings.TrimSpace(origin), "/"); origin != "" {
			origins = append(origins, origin)
		}
	}
	c.CORS.AllowedOrigins = origins
	if c.MaxRequestBodyBytes < 1 {
		c.MaxRequestBodyBytes = 105 * 1024 * 1024
	}
	for i := range c.APIKeys {
		if c.APIKeys[i].Name == "" {
			c.APIKeys[i].Name = fmt.Sprintf("key-%d", i+1)
		}
		if !c.APIKeys[i].Enabled && c.APIKeys[i].Key == "" && c.APIKeys[i].KeyHash == "" {
			c.APIKeys[i].Enabled = true
		}
		if c.APIKeys[i].CreatedAt == "" {
			c.APIKeys[i].CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
}

// Load reads YAML or JSON and creates a safe default file on first start.
// The returned strings are secrets that were generated during this call and
// should be printed once by the caller.
func Load(path string) (*Config, []string, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".json" {
		return loadJSON(path)
	}
	return loadYAML(path)
}

func loadYAML(path string) (*Config, []string, error) {
	cfg := Default()
	if abs, err := filepath.Abs(path); err == nil {
		cfg.baseDir = filepath.Dir(abs)
	}
	if data, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, nil, fmt.Errorf("parse config: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	applyEnvOverrides(&cfg)
	cfg.Normalize()
	generated, changed, err := ensureSecrets(&cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cfg.DataDirPath(), 0o755); err != nil {
		return nil, nil, err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) || changed || len(generated) > 0 {
		if err := SaveYAML(path, &cfg); err != nil {
			return nil, nil, err
		}
	}
	return &cfg, generated, nil
}

func loadJSON(path string) (*Config, []string, error) {
	cfg := Default()
	if abs, err := filepath.Abs(path); err == nil {
		cfg.baseDir = filepath.Dir(abs)
	}
	if data, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, nil, fmt.Errorf("parse config: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	applyEnvOverrides(&cfg)
	cfg.Normalize()
	generated, changed, err := ensureSecrets(&cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cfg.DataDirPath(), 0o755); err != nil {
		return nil, nil, err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) || changed || len(generated) > 0 {
		if err := SaveJSON(path, &cfg); err != nil {
			return nil, nil, err
		}
	}
	return &cfg, generated, nil
}

func ensureSecrets(c *Config) ([]string, bool, error) {
	var generated []string
	changed := false
	if strings.TrimSpace(c.ManagementSecretKey()) == "" {
		raw := os.Getenv("CHATGPT2API_MANAGEMENT_KEY")
		if strings.TrimSpace(raw) == "" {
			raw = randomSecret("mgmt_")
			generated = append(generated, "management-key="+raw)
		}
		hash, err := HashSecret(raw)
		if err != nil {
			return nil, false, err
		}
		c.RemoteManagement.SecretKey = hash
		changed = true
	} else if !isBcryptHash(c.ManagementSecretKey()) {
		hash, err := HashSecret(c.ManagementSecretKey())
		if err != nil {
			return nil, false, err
		}
		c.RemoteManagement.SecretKey = hash
		changed = true
	}
	if len(c.APIKeys) == 0 {
		raw := randomSecret("sk-")
		hash := HashAPIKey(raw)
		c.APIKeys = append(c.APIKeys, APIKey{
			Name:      "default",
			KeyHash:   hash,
			Enabled:   true,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		})
		generated = append(generated, "api-key="+raw)
		changed = true
	}
	for i := range c.APIKeys {
		if strings.TrimSpace(c.APIKeys[i].Key) == "" {
			continue
		}
		c.APIKeys[i].KeyHash = HashAPIKey(c.APIKeys[i].Key)
		c.APIKeys[i].Key = ""
		changed = true
	}
	if strings.TrimSpace(c.SigningKey) == "" {
		c.SigningKey = randomSecret("sign_")
		changed = true
	}
	return generated, changed, nil
}

func applyEnvOverrides(c *Config) {
	if value := strings.TrimSpace(os.Getenv("SENTINEL_BROWSER_BIN")); value != "" {
		c.Sentinel.BrowserPath = value
	}
	if value := strings.TrimSpace(os.Getenv("SENTINEL_SOLVER_TIMEZONE")); value != "" {
		c.Sentinel.Timezone = value
	}
}

func ValidateProxyURL(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}
	if parsed.Host == "" {
		return errors.New("proxy URL must include a host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("proxy URL scheme must be http, https, socks5, or socks5h")
	}
}

func (c *Config) AllowsOrigin(origin string) bool {
	origin = strings.TrimSuffix(strings.TrimSpace(origin), "/")
	if origin == "" {
		return false
	}
	parsedOrigin, originErr := url.Parse(origin)
	for _, value := range c.CORS.AllowedOrigins {
		pattern := strings.TrimSuffix(strings.TrimSpace(value), "/")
		if pattern == "*" {
			return true
		}
		if strings.EqualFold(pattern, origin) {
			return true
		}
		if originErr != nil {
			continue
		}
		parsedPattern, err := url.Parse(pattern)
		if err != nil || !strings.EqualFold(parsedPattern.Scheme, parsedOrigin.Scheme) || parsedPattern.Port() != parsedOrigin.Port() {
			continue
		}
		hostPattern := strings.ToLower(parsedPattern.Hostname())
		if !strings.HasPrefix(hostPattern, "*.") {
			continue
		}
		suffix := strings.TrimPrefix(hostPattern, "*")
		host := strings.ToLower(parsedOrigin.Hostname())
		if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func isBcryptHash(value string) bool {
	return strings.HasPrefix(value, "$2a$") || strings.HasPrefix(value, "$2b$") || strings.HasPrefix(value, "$2y$")
}

func randomSecret(prefix string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf)
}

func HashSecret(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("secret is empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(value), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func HashAPIKey(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func VerifySecret(stored, candidate string) bool {
	stored = strings.TrimSpace(stored)
	candidate = strings.TrimSpace(candidate)
	if stored == "" || candidate == "" {
		return false
	}
	if strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") || strings.HasPrefix(stored, "$2y$") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(candidate)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1
}

func VerifyAPIKey(item APIKey, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || !item.Enabled {
		return false
	}
	if item.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, item.ExpiresAt); err == nil && time.Now().After(t) {
			return false
		}
	}
	return MatchesAPIKey(item, candidate)
}

func MatchesAPIKey(item APIKey, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	if strings.TrimSpace(item.KeyHash) != "" {
		return subtle.ConstantTimeCompare([]byte(strings.ToLower(item.KeyHash)), []byte(strings.ToLower(HashAPIKey(candidate)))) == 1
	}
	if strings.TrimSpace(item.Key) != "" {
		return subtle.ConstantTimeCompare([]byte(item.Key), []byte(candidate)) == 1
	}
	return false
}

func MaskAPIKey(item APIKey) string {
	if item.Key != "" && len(item.Key) > 8 {
		return item.Key[:3] + "****" + item.Key[len(item.Key)-4:]
	}
	if item.KeyHash != "" {
		if len(item.KeyHash) > 12 {
			return "hash:" + item.KeyHash[:12] + "…"
		}
		return item.KeyHash
	}
	return ""
}

func (c *Config) ManagementSecretKey() string {
	return strings.TrimSpace(c.RemoteManagement.SecretKey)
}

func (c *Config) ManagementEnabled() bool {
	return c.ManagementSecretKey() != ""
}

func (c *Config) DBPath() string {
	return filepath.Join(c.DataDirPath(), "chatgpt2api.db")
}

func (c *Config) DataDirPath() string {
	return c.resolvePath(c.DataDir)
}

func (c *Config) AuthDirPath() string {
	return c.resolvePath(c.AuthDir)
}

func (c *Config) ResolvePath(value string) string {
	return c.resolvePath(value)
}

func (c *Config) resolvePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	if c.baseDir == "" {
		return filepath.Clean(value)
	}
	return filepath.Join(c.baseDir, value)
}

type Store struct {
	mu     sync.RWMutex
	path   string
	config Config
}

func NewStore(path string, cfg *Config) *Store {
	return &Store{path: path, config: *cfg}
}

func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.config
	fn(&next)
	next.Normalize()
	if err := savePath(s.path, &next); err != nil {
		return err
	}
	s.config = next
	return nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return savePath(s.path, &s.config)
}

func savePath(path string, cfg *Config) error {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return SaveJSON(path, cfg)
	}
	return SaveYAML(path, cfg)
}

func SaveYAML(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func SaveJSON(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
