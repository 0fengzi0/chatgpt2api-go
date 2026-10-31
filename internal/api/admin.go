package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, _ := s.db.ListAPIKeyUsage(r.Context())
	cfg := s.cfg.Snapshot()
	active, waiting := s.limiter.Stats()
	var solver any
	if s.solverStatus != nil {
		solver = s.solverStatus()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"stats":  stats,
		"runtime": map[string]any{
			"active_requests": active,
			"queued_requests": waiting,
			"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
		},
		"config": map[string]any{
			"management":                cfg.ManagementEnabled(),
			"global_concurrency":        cfg.Performance.ImageWorkers,
			"account_concurrency":       cfg.Image.AccountConcurrency,
			"request_timeout_seconds":   cfg.Image.RequestTimeoutSeconds,
			"temporary_url_ttl_seconds": cfg.Image.TemporaryURLTTLSeconds,
			"log_retention_days":        cfg.Image.LogRetentionDays,
			"api_keys":                  len(cfg.APIKeys),
		},
		"api_key_usage": usage,
		"solver":        solver,
	})
}

func (s *Server) accessSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeAccessSettings(w)
	case http.MethodPut:
		s.updateAccessSettings(w, r)
	default:
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) updateAccessSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ManagementKey           string            `json:"management_key"`
		Host                    *string           `json:"host"`
		Port                    *int              `json:"port"`
		Debug                   *bool             `json:"debug"`
		UsageStatistics         *bool             `json:"usage_statistics_enabled"`
		LoggingToFile           *bool             `json:"logging_to_file"`
		ProxyURL                *string           `json:"proxy_url"`
		PublicBaseURL           *string           `json:"public_base_url"`
		ProviderBaseURL         *string           `json:"provider_base_url"`
		GlobalConcurrency       *int              `json:"global_concurrency"`
		AccountConcurrency      *int              `json:"account_concurrency"`
		RequestTimeout          *int              `json:"request_timeout_seconds"`
		TemporaryURLTTL         *int              `json:"temporary_url_ttl_seconds"`
		PollTimeout             *int              `json:"image_poll_timeout_seconds"`
		PollInterval            *int              `json:"image_poll_interval_seconds"`
		QueueSize               *int              `json:"image_queue_size"`
		LogRetentionDays        *int              `json:"log_retention_days"`
		MaxUploadBytes          *int64            `json:"max_upload_bytes"`
		MaxImageBytes           *int64            `json:"max_image_bytes"`
		MaxImagePixels          *int64            `json:"max_image_pixels"`
		MaxDimension            *int              `json:"max_dimension"`
		ParallelGeneration      *bool             `json:"parallel_generation"`
		AccountRefreshWorkers   *int              `json:"account_refresh_workers"`
		TextCacheEntries        *int              `json:"text_cache_entries"`
		MetricsMode             *string           `json:"metrics_mode"`
		QuotaSwitchAccount      *bool             `json:"quota_switch_account"`
		QuotaSwitchModel        *bool             `json:"quota_switch_model"`
		BrowserPath             *string           `json:"sentinel_browser_path"`
		Timezone                *string           `json:"sentinel_timezone"`
		SentinelHeadless        *bool             `json:"sentinel_headless"`
		SentinelNoSandbox       *bool             `json:"sentinel_no_sandbox"`
		SentinelRefreshMinutes  *int              `json:"sentinel_refresh_minutes"`
		SentinelNavigateTimeout *int              `json:"sentinel_navigate_timeout_seconds"`
		SentinelSolveTimeout    *int              `json:"sentinel_solve_timeout_seconds"`
		DefaultUpstreamModel    *string           `json:"default_upstream_model"`
		ImageGenerationModel    *string           `json:"image_generation_model"`
		DefaultThinkingEffort   *string           `json:"default_thinking_effort"`
		RefreshInterval         *int              `json:"refresh_interval_minutes"`
		AutoRemoveInvalid       *bool             `json:"auto_remove_invalid_accounts"`
		SensitiveWords          *[]string         `json:"sensitive_words"`
		CORSAllowedOrigins      *[]string         `json:"cors_allowed_origins"`
		MaxRequestBodyBytes     *int64            `json:"max_request_body_bytes"`
		OpenAICompatibility     *[]map[string]any `json:"openai_compatibility"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeManagementError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Port != nil && (*body.Port < 1 || *body.Port > 65535) {
		writeManagementError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}
	if body.GlobalConcurrency != nil && (*body.GlobalConcurrency < 1 || *body.GlobalConcurrency > 64) {
		writeManagementError(w, http.StatusBadRequest, "global_concurrency must be between 1 and 64")
		return
	}
	if body.AccountConcurrency != nil && (*body.AccountConcurrency < 1 || *body.AccountConcurrency > 16) {
		writeManagementError(w, http.StatusBadRequest, "account_concurrency must be between 1 and 16")
		return
	}
	if body.RequestTimeout != nil && (*body.RequestTimeout < 30 || *body.RequestTimeout > 3600) {
		writeManagementError(w, http.StatusBadRequest, "request_timeout_seconds must be between 30 and 3600")
		return
	}
	if body.TemporaryURLTTL != nil && (*body.TemporaryURLTTL < 60 || *body.TemporaryURLTTL > 86400) {
		writeManagementError(w, http.StatusBadRequest, "temporary_url_ttl_seconds must be between 60 and 86400")
		return
	}
	if body.LogRetentionDays != nil && (*body.LogRetentionDays < 1 || *body.LogRetentionDays > 3650) {
		writeManagementError(w, http.StatusBadRequest, "log_retention_days must be between 1 and 3650")
		return
	}
	if body.RefreshInterval != nil && (*body.RefreshInterval < 1 || *body.RefreshInterval > 10080) {
		writeManagementError(w, http.StatusBadRequest, "refresh_interval_minutes must be between 1 and 10080")
		return
	}
	if body.PollTimeout != nil && (*body.PollTimeout < 5 || *body.PollTimeout > 3600) {
		writeManagementError(w, http.StatusBadRequest, "image_poll_timeout_seconds must be between 5 and 3600")
		return
	}
	if body.PollInterval != nil && (*body.PollInterval < 1 || *body.PollInterval > 300) {
		writeManagementError(w, http.StatusBadRequest, "image_poll_interval_seconds must be between 1 and 300")
		return
	}
	if body.QueueSize != nil && (*body.QueueSize < 1 || *body.QueueSize > 10000) {
		writeManagementError(w, http.StatusBadRequest, "image_queue_size must be between 1 and 10000")
		return
	}
	if body.MaxUploadBytes != nil && (*body.MaxUploadBytes < 64*1024 || *body.MaxUploadBytes > 1024*1024*1024) {
		writeManagementError(w, http.StatusBadRequest, "max_upload_bytes must be between 65536 and 1073741824")
		return
	}
	if body.MaxImageBytes != nil && (*body.MaxImageBytes < 64*1024 || *body.MaxImageBytes > 1024*1024*1024) {
		writeManagementError(w, http.StatusBadRequest, "max_image_bytes must be between 65536 and 1073741824")
		return
	}
	if body.MaxImagePixels != nil && (*body.MaxImagePixels < config.MinimumSupportedImagePixels || *body.MaxImagePixels > 128*1024*1024) {
		writeManagementError(w, http.StatusBadRequest, "max_image_pixels must be between 8294400 and 134217728")
		return
	}
	if body.MaxDimension != nil && (*body.MaxDimension < config.MinimumSupportedImageDimension || *body.MaxDimension > 16384) {
		writeManagementError(w, http.StatusBadRequest, "max_dimension must be between 3840 and 16384")
		return
	}
	if body.AccountRefreshWorkers != nil && (*body.AccountRefreshWorkers < 1 || *body.AccountRefreshWorkers > 64) {
		writeManagementError(w, http.StatusBadRequest, "account_refresh_workers must be between 1 and 64")
		return
	}
	if body.TextCacheEntries != nil && (*body.TextCacheEntries < 0 || *body.TextCacheEntries > 1000000) {
		writeManagementError(w, http.StatusBadRequest, "text_cache_entries must be between 0 and 1000000")
		return
	}
	if body.SentinelRefreshMinutes != nil && (*body.SentinelRefreshMinutes < 1 || *body.SentinelRefreshMinutes > 1440) {
		writeManagementError(w, http.StatusBadRequest, "sentinel_refresh_minutes must be between 1 and 1440")
		return
	}
	if body.SentinelNavigateTimeout != nil && (*body.SentinelNavigateTimeout < 5 || *body.SentinelNavigateTimeout > 600) {
		writeManagementError(w, http.StatusBadRequest, "sentinel_navigate_timeout_seconds must be between 5 and 600")
		return
	}
	if body.SentinelSolveTimeout != nil && (*body.SentinelSolveTimeout < 5 || *body.SentinelSolveTimeout > 600) {
		writeManagementError(w, http.StatusBadRequest, "sentinel_solve_timeout_seconds must be between 5 and 600")
		return
	}
	if body.MaxRequestBodyBytes != nil && (*body.MaxRequestBodyBytes < 64*1024 || *body.MaxRequestBodyBytes > 1024*1024*1024) {
		writeManagementError(w, http.StatusBadRequest, "max_request_body_bytes must be between 65536 and 1073741824")
		return
	}
	if body.MetricsMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*body.MetricsMode))
		if mode != "light" && mode != "full" && mode != "off" {
			writeManagementError(w, http.StatusBadRequest, "metrics_mode must be light, full, or off")
			return
		}
	}
	if body.DefaultThinkingEffort != nil {
		effort := strings.ToLower(strings.TrimSpace(*body.DefaultThinkingEffort))
		switch effort {
		case "auto", "minimal", "low", "medium", "high", "standard", "extended", "max":
		default:
			writeManagementError(w, http.StatusBadRequest, "default_thinking_effort is invalid")
			return
		}
	}
	normalizedHost := ""
	if body.Host != nil {
		normalizedHost = strings.TrimSpace(*body.Host)
		if normalizedHost == "" {
			writeManagementError(w, http.StatusBadRequest, "host is required")
			return
		}
	}
	normalizedProviderBaseURL := ""
	if body.ProviderBaseURL != nil {
		normalizedProviderBaseURL = strings.TrimRight(strings.TrimSpace(*body.ProviderBaseURL), "/")
		if !isAbsoluteHTTPURL(normalizedProviderBaseURL) {
			writeManagementError(w, http.StatusBadRequest, "provider_base_url must be an absolute http or https URL")
			return
		}
	}
	normalizedOrigins := []string(nil)
	if body.CORSAllowedOrigins != nil {
		var err error
		normalizedOrigins, err = normalizeCORSOrigins(*body.CORSAllowedOrigins)
		if err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	normalizedSensitiveWords := []string(nil)
	if body.SensitiveWords != nil {
		normalizedSensitiveWords = normalizeStringList(*body.SensitiveWords)
	}
	normalizedProxy := ""
	if body.ProxyURL != nil {
		normalizedProxy = strings.TrimSpace(*body.ProxyURL)
		if err := config.ValidateProxyURL(normalizedProxy); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.PublicBaseURL != nil {
		value := strings.TrimRight(strings.TrimSpace(*body.PublicBaseURL), "/")
		if value != "" {
			parsed, err := url.Parse(value)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				writeManagementError(w, http.StatusBadRequest, "public_base_url must be an absolute http or https URL")
				return
			}
		}
	}
	if err := s.cfg.Update(func(c *config.Config) {
		if body.Host != nil {
			c.Host = normalizedHost
		}
		if body.Port != nil {
			c.Port = *body.Port
		}
		if body.Debug != nil {
			c.Debug = *body.Debug
		}
		if body.UsageStatistics != nil {
			c.UsageStatistics = *body.UsageStatistics
		}
		if body.LoggingToFile != nil {
			c.LoggingToFile = *body.LoggingToFile
		}
		if body.ProxyURL != nil {
			c.ProxyURL = normalizedProxy
		}
		if body.ProviderBaseURL != nil {
			c.ProviderBaseURL = normalizedProviderBaseURL
		}
		if body.GlobalConcurrency != nil {
			c.Performance.ImageWorkers = *body.GlobalConcurrency
		}
		if body.AccountConcurrency != nil {
			c.Image.AccountConcurrency = *body.AccountConcurrency
		}
		if body.RequestTimeout != nil {
			c.Image.RequestTimeoutSeconds = *body.RequestTimeout
		}
		if body.TemporaryURLTTL != nil {
			c.Image.TemporaryURLTTLSeconds = *body.TemporaryURLTTL
		}
		if body.PollTimeout != nil {
			c.Image.PollTimeoutSeconds = *body.PollTimeout
		}
		if body.PollInterval != nil {
			c.Image.PollIntervalSeconds = *body.PollInterval
		}
		if body.QueueSize != nil {
			c.Image.QueueSize = *body.QueueSize
		}
		if body.LogRetentionDays != nil {
			c.Image.LogRetentionDays = *body.LogRetentionDays
		}
		if body.MaxUploadBytes != nil {
			c.Image.MaxUploadBytes = *body.MaxUploadBytes
		}
		if body.MaxImageBytes != nil {
			c.Image.MaxImageBytes = *body.MaxImageBytes
		}
		if body.MaxImagePixels != nil {
			c.Image.MaxImagePixels = *body.MaxImagePixels
		}
		if body.MaxDimension != nil {
			c.Image.MaxDimension = *body.MaxDimension
		}
		if body.ParallelGeneration != nil {
			c.Performance.ParallelGeneration = *body.ParallelGeneration
		}
		if body.AccountRefreshWorkers != nil {
			c.Performance.AccountRefreshWorkers = *body.AccountRefreshWorkers
		}
		if body.TextCacheEntries != nil {
			c.Performance.TextCacheEntries = *body.TextCacheEntries
		}
		if body.MetricsMode != nil {
			c.Performance.MetricsMode = strings.ToLower(strings.TrimSpace(*body.MetricsMode))
		}
		if body.QuotaSwitchAccount != nil {
			c.QuotaExceeded.SwitchAccount = *body.QuotaSwitchAccount
		}
		if body.QuotaSwitchModel != nil {
			c.QuotaExceeded.SwitchModel = *body.QuotaSwitchModel
		}
		if body.BrowserPath != nil {
			c.Sentinel.BrowserPath = strings.TrimSpace(*body.BrowserPath)
		}
		if body.Timezone != nil {
			c.Sentinel.Timezone = strings.TrimSpace(*body.Timezone)
		}
		if body.SentinelHeadless != nil {
			c.Sentinel.Headless = *body.SentinelHeadless
		}
		if body.SentinelNoSandbox != nil {
			c.Sentinel.NoSandbox = *body.SentinelNoSandbox
		}
		if body.SentinelRefreshMinutes != nil {
			c.Sentinel.RefreshMinutes = *body.SentinelRefreshMinutes
		}
		if body.SentinelNavigateTimeout != nil {
			c.Sentinel.NavigateTimeoutSecs = *body.SentinelNavigateTimeout
		}
		if body.SentinelSolveTimeout != nil {
			c.Sentinel.SolveTimeoutSeconds = *body.SentinelSolveTimeout
		}
		if body.PublicBaseURL != nil {
			c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(*body.PublicBaseURL), "/")
		}
		if body.DefaultUpstreamModel != nil {
			c.DefaultUpstreamModel = strings.TrimSpace(*body.DefaultUpstreamModel)
		}
		if body.ImageGenerationModel != nil {
			c.ImageGenerationModel = strings.TrimSpace(*body.ImageGenerationModel)
		}
		if body.DefaultThinkingEffort != nil {
			c.DefaultThinkingEffort = strings.ToLower(strings.TrimSpace(*body.DefaultThinkingEffort))
		}
		if body.RefreshInterval != nil {
			c.RefreshIntervalMinute = *body.RefreshInterval
		}
		if body.AutoRemoveInvalid != nil {
			c.Image.AutoRemoveInvalidAccounts = *body.AutoRemoveInvalid
		}
		if body.SensitiveWords != nil {
			c.SensitiveWords = append([]string(nil), normalizedSensitiveWords...)
		}
		if body.CORSAllowedOrigins != nil {
			c.CORS.AllowedOrigins = append([]string(nil), normalizedOrigins...)
		}
		if body.MaxRequestBodyBytes != nil {
			c.MaxRequestBodyBytes = *body.MaxRequestBodyBytes
		}
		if body.OpenAICompatibility != nil {
			c.OpenAICompatibility = append([]map[string]any(nil), (*body.OpenAICompatibility)...)
		}
	}); err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.ProxyURL != nil && s.proxyUpdater != nil {
		s.proxyUpdater(normalizedProxy)
	}
	if strings.TrimSpace(body.ManagementKey) != "" {
		if err := s.auth.UpdateManagementKey(body.ManagementKey); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.writeAccessSettings(w)
}

func (s *Server) writeAccessSettings(w http.ResponseWriter) {
	cfg := s.cfg.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"management_enabled":                cfg.ManagementEnabled(),
		"host":                              cfg.Host,
		"port":                              cfg.Port,
		"data_dir":                          cfg.DataDir,
		"auth_dir":                          cfg.AuthDir,
		"signing_key_configured":            strings.TrimSpace(cfg.SigningKey) != "",
		"debug":                             cfg.Debug,
		"usage_statistics_enabled":          cfg.UsageStatistics,
		"logging_to_file":                   cfg.LoggingToFile,
		"proxy_url":                         cfg.ProxyURL,
		"global_concurrency":                cfg.Performance.ImageWorkers,
		"account_concurrency":               cfg.Image.AccountConcurrency,
		"request_timeout_seconds":           cfg.Image.RequestTimeoutSeconds,
		"temporary_url_ttl_seconds":         cfg.Image.TemporaryURLTTLSeconds,
		"image_poll_timeout_seconds":        cfg.Image.PollTimeoutSeconds,
		"image_poll_interval_seconds":       cfg.Image.PollIntervalSeconds,
		"image_queue_size":                  cfg.Image.QueueSize,
		"log_retention_days":                cfg.Image.LogRetentionDays,
		"max_upload_bytes":                  cfg.Image.MaxUploadBytes,
		"max_image_bytes":                   cfg.Image.MaxImageBytes,
		"max_image_pixels":                  cfg.Image.MaxImagePixels,
		"max_dimension":                     cfg.Image.MaxDimension,
		"parallel_generation":               cfg.Performance.ParallelGeneration,
		"account_refresh_workers":           cfg.Performance.AccountRefreshWorkers,
		"text_cache_entries":                cfg.Performance.TextCacheEntries,
		"metrics_mode":                      cfg.Performance.MetricsMode,
		"quota_switch_account":              cfg.QuotaExceeded.SwitchAccount,
		"quota_switch_model":                cfg.QuotaExceeded.SwitchModel,
		"public_base_url":                   cfg.PublicBaseURL,
		"provider_base_url":                 cfg.ProviderBaseURL,
		"image_generation_model":            cfg.ImageGenerationModel,
		"default_upstream_model":            cfg.DefaultUpstreamModel,
		"default_thinking_effort":           cfg.DefaultThinkingEffort,
		"sentinel_browser_path":             cfg.Sentinel.BrowserPath,
		"sentinel_timezone":                 cfg.Sentinel.Timezone,
		"sentinel_headless":                 cfg.Sentinel.Headless,
		"sentinel_no_sandbox":               cfg.Sentinel.NoSandbox,
		"sentinel_refresh_minutes":          cfg.Sentinel.RefreshMinutes,
		"sentinel_navigate_timeout_seconds": cfg.Sentinel.NavigateTimeoutSecs,
		"sentinel_solve_timeout_seconds":    cfg.Sentinel.SolveTimeoutSeconds,
		"refresh_interval_minutes":          cfg.RefreshIntervalMinute,
		"auto_remove_invalid_accounts":      cfg.Image.AutoRemoveInvalidAccounts,
		"sensitive_words":                   cfg.SensitiveWords,
		"cors_allowed_origins":              cfg.CORS.AllowedOrigins,
		"max_request_body_bytes":            cfg.MaxRequestBodyBytes,
		"openai_compatibility":              cfg.OpenAICompatibility,
		"minimum_image_pixels":              config.MinimumSupportedImagePixels,
		"minimum_image_dimension":           config.MinimumSupportedImageDimension,
	})
}

func isAbsoluteHTTPURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func normalizeStringList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func normalizeCORSOrigins(values []string) ([]string, error) {
	origins := normalizeStringList(values)
	for index, origin := range origins {
		origin = strings.TrimSuffix(origin, "/")
		if origin == "*" {
			origins[index] = origin
			continue
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("invalid CORS origin: %s", origin)
		}
		origins[index] = origin
	}
	return origins, nil
}

func (s *Server) apiKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.cfg.Snapshot()
		usage, _ := s.db.ListAPIKeyUsage(r.Context())
		usageByName := make(map[string]store.APIKeyUsage, len(usage))
		for _, item := range usage {
			usageByName[item.Name] = item
		}
		items := make([]map[string]any, 0, len(cfg.APIKeys))
		for _, item := range cfg.APIKeys {
			lastUsed := ""
			requests := int64(0)
			failures := int64(0)
			if value, ok := usageByName[item.Name]; ok {
				lastUsed = value.LastUsedAt
				requests = value.Requests
				failures = value.Failures
			}
			items = append(items, map[string]any{
				"name": item.Name, "masked": config.MaskAPIKey(item), "enabled": item.Enabled,
				"expires_at": item.ExpiresAt, "created_at": item.CreatedAt,
				"last_used_at": lastUsed, "requests": requests, "failures": failures,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
		raw, err := s.auth.CreateAPIKey(body.Name)
		if err != nil {
			writeManagementError(w, http.StatusConflict, err.Error())
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = "client"
		}
		writeJSON(w, http.StatusCreated, map[string]any{"key": raw, "name": name})
	default:
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) apiKeyByID(w http.ResponseWriter, r *http.Request) {
	name, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/access/keys/"))
	if err != nil || strings.TrimSpace(name) == "" {
		writeManagementError(w, http.StatusBadRequest, "key name is required")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.auth.DeleteAPIKey(name); err != nil {
			writeManagementError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPatch:
		var body struct {
			Enabled *bool  `json:"enabled"`
			Name    string `json:"name"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.auth.UpdateAPIKey(name, body.Enabled, body.Name); err != nil {
			writeManagementError(w, http.StatusConflict, err.Error())
			return
		}
		if strings.TrimSpace(body.Name) != "" && !strings.EqualFold(strings.TrimSpace(body.Name), name) {
			_ = s.db.RenameAPIKeyUsage(r.Context(), name, body.Name)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) accountsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.accounts.List(r.Context())
		if err != nil {
			writeManagementError(w, http.StatusInternalServerError, err.Error())
			return
		}
		public := make([]store.Account, 0, len(items))
		for _, item := range items {
			public = append(public, item.Public())
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": public})
	case http.MethodPost:
		var body struct {
			Tokens   []string             `json:"tokens"`
			Accounts []account.ImportItem `json:"accounts"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.accounts.Import(r.Context(), body.Tokens, body.Accounts)
		if err != nil {
			writeManagementError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
		removed, err := s.accounts.Delete(r.Context(), body.IDs)
		if err != nil {
			writeManagementError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
	default:
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) refreshAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	result := s.accounts.RefreshMany(r.Context(), body.IDs)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) accountAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		writeManagementError(w, http.StatusBadRequest, "account id is required")
		return
	}
	id, _ := url.PathUnescape(parts[0])
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var updates map[string]any
		if err := decodeJSON(r, &updates); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
		if status, ok := updates["status"].(string); ok && !contains([]string{
			store.AccountNormal, store.AccountLimited, store.AccountError, store.AccountDisabled,
		}, status) {
			writeManagementError(w, http.StatusBadRequest, "invalid account status")
			return
		}
		if proxy, ok := updates["proxy"].(string); ok {
			if err := config.ValidateProxyURL(proxy); err != nil {
				writeManagementError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		item, err := s.accounts.Update(r.Context(), id, updates)
		if err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"item": item.Public()})
		return
	}
	if len(parts) == 2 && parts[1] == "refresh" && r.Method == http.MethodPost {
		if err := s.accounts.RefreshOne(r.Context(), id); err != nil {
			writeManagementError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeManagementError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	items, err := s.accounts.Export(r.Context(), body.IDs)
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var payload any = items
	if len(items) == 1 {
		payload = items[0]
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="accounts.json"`)
	_, _ = w.Write(data)
}

func (s *Server) importAccountJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 20<<20+1))
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, "read account JSON: "+err.Error())
		return
	}
	if len(data) > 20<<20 {
		writeManagementError(w, http.StatusRequestEntityTooLarge, "account JSON is too large")
		return
	}
	items, err := account.ParseImportJSON(data)
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.accounts.Import(r.Context(), nil, items)
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	query := r.URL.Query()
	items, err := s.db.ListRequestLogsFiltered(r.Context(), store.RequestLogFilter{
		Limit: limit, Owner: query.Get("owner"), Model: query.Get("model"),
		Mode: query.Get("mode"), Status: query.Get("status"),
		CreatedFrom: normalizeLogTime(query.Get("start"), false),
		CreatedTo:   normalizeLogTime(query.Get("end"), true),
	})
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, _ := s.db.ListAPIKeyUsage(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "api_key_usage": usage})
}

func (s *Server) accountEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.db.ListAccountEvents(r.Context(), r.URL.Query().Get("account_id"), limit)
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func normalizeLogTime(value string, endOfDay bool) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, err := time.Parse("2006-01-02", value); err == nil {
		if endOfDay {
			return value + "T23:59:59Z"
		}
		return value + "T00:00:00Z"
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC().Format(time.RFC3339)
	}
	return value
}

func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		IDs    []int64 `json:"ids"`
		Before string  `json:"before"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeManagementError(w, http.StatusBadRequest, err.Error())
		return
	}
	var removed int64
	var err error
	if strings.TrimSpace(body.Before) != "" {
		removed, err = s.db.PurgeRequestLogsBefore(r.Context(), strings.TrimSpace(body.Before))
	} else {
		removed, err = s.db.DeleteRequestLogs(r.Context(), body.IDs)
	}
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func parseAccountStatus(value string) error {
	switch value {
	case "", store.AccountNormal, store.AccountLimited, store.AccountError, store.AccountDisabled:
		return nil
	default:
		return errors.New("invalid account status")
	}
}
