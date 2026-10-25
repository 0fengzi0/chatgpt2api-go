package api

import (
	"encoding/json"
	"errors"
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
		ManagementKey        string   `json:"management_key"`
		Port                 *int     `json:"port"`
		ProxyURL             *string  `json:"proxy_url"`
		GlobalConcurrency    *int     `json:"global_concurrency"`
		AccountConcurrency   *int     `json:"account_concurrency"`
		RequestTimeout       *int     `json:"request_timeout_seconds"`
		TemporaryURLTTL      *int     `json:"temporary_url_ttl_seconds"`
		LogRetentionDays     *int     `json:"log_retention_days"`
		BrowserPath          *string  `json:"sentinel_browser_path"`
		Timezone             *string  `json:"sentinel_timezone"`
		PublicBaseURL        *string  `json:"public_base_url"`
		DefaultUpstreamModel *string  `json:"default_upstream_model"`
		RefreshInterval      *int     `json:"refresh_interval_minutes"`
		AutoRemoveInvalid    *bool    `json:"auto_remove_invalid_accounts"`
		SensitiveWords       []string `json:"sensitive_words"`
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
		if body.Port != nil {
			c.Port = *body.Port
		}
		if body.ProxyURL != nil {
			c.ProxyURL = normalizedProxy
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
		if body.LogRetentionDays != nil {
			c.Image.LogRetentionDays = *body.LogRetentionDays
		}
		if body.BrowserPath != nil {
			c.Sentinel.BrowserPath = strings.TrimSpace(*body.BrowserPath)
		}
		if body.Timezone != nil {
			c.Sentinel.Timezone = strings.TrimSpace(*body.Timezone)
		}
		if body.PublicBaseURL != nil {
			c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(*body.PublicBaseURL), "/")
		}
		if body.DefaultUpstreamModel != nil {
			c.DefaultUpstreamModel = strings.TrimSpace(*body.DefaultUpstreamModel)
		}
		if body.RefreshInterval != nil {
			c.RefreshIntervalMinute = *body.RefreshInterval
		}
		if body.AutoRemoveInvalid != nil {
			c.Image.AutoRemoveInvalidAccounts = *body.AutoRemoveInvalid
		}
		if body.SensitiveWords != nil {
			c.SensitiveWords = append([]string(nil), body.SensitiveWords...)
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
		"management_enabled":           cfg.ManagementEnabled(),
		"host":                         cfg.Host,
		"port":                         cfg.Port,
		"proxy_url":                    cfg.ProxyURL,
		"data_dir":                     cfg.DataDir,
		"global_concurrency":           cfg.Performance.ImageWorkers,
		"account_concurrency":          cfg.Image.AccountConcurrency,
		"request_timeout_seconds":      cfg.Image.RequestTimeoutSeconds,
		"temporary_url_ttl_seconds":    cfg.Image.TemporaryURLTTLSeconds,
		"log_retention_days":           cfg.Image.LogRetentionDays,
		"public_base_url":              cfg.PublicBaseURL,
		"sentinel_browser_path":        cfg.Sentinel.BrowserPath,
		"sentinel_timezone":            cfg.Sentinel.Timezone,
		"default_upstream_model":       cfg.DefaultUpstreamModel,
		"refresh_interval_minutes":     cfg.RefreshIntervalMinute,
		"auto_remove_invalid_accounts": cfg.Image.AutoRemoveInvalidAccounts,
		"sensitive_words":              cfg.SensitiveWords,
		"max_upload_bytes":             cfg.Image.MaxUploadBytes,
		"max_image_pixels":             cfg.Image.MaxImagePixels,
		"max_dimension":                cfg.Image.MaxDimension,
	})
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
