package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/auth"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/image"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
	"github.com/0fengzi0/chatgpt2api-go/internal/web"
)

type Server struct {
	cfg          *config.Store
	db           *store.Store
	accounts     *account.Service
	auth         *auth.Manager
	storage      *image.Storage
	provider     provider.Provider
	logger       *slog.Logger
	mux          *http.ServeMux
	proxyUpdater func(string)
}

type Option func(*Server)

func WithProxyUpdater(update func(string)) Option {
	return func(server *Server) {
		server.proxyUpdater = update
	}
}

func New(cfg *config.Store, db *store.Store, accounts *account.Service, authManager *auth.Manager, storage *image.Storage, p provider.Provider, logger *slog.Logger, options ...Option) *Server {
	s := &Server{cfg: cfg, db: db, accounts: accounts, auth: authManager, storage: storage, provider: p, logger: logger, mux: http.NewServeMux()}
	for _, option := range options {
		option(s)
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.middleware(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/metrics", s.metrics)
	s.mux.HandleFunc("/version", s.version)
	s.mux.HandleFunc("/auth/login", s.authLogin)
	s.mux.HandleFunc("/admin", s.adminAsset)
	s.mux.HandleFunc("/admin/", s.adminAsset)
	s.mux.HandleFunc("/api/status", s.management(s.status))
	s.mux.HandleFunc("/api/access/settings", s.management(s.accessSettings))
	s.mux.HandleFunc("/api/access/keys", s.management(s.apiKeys))
	s.mux.HandleFunc("/api/access/keys/", s.management(s.apiKeyByID))
	s.mux.HandleFunc("/api/accounts", s.management(s.accountsHandler))
	s.mux.HandleFunc("/api/accounts/refresh", s.management(s.refreshAccounts))
	s.mux.HandleFunc("/api/accounts/export", s.management(s.exportAccounts))
	s.mux.HandleFunc("/api/accounts/import-dir", s.management(s.importAccountDir))
	s.mux.HandleFunc("/api/accounts/import-json", s.management(s.importAccountJSON))
	s.mux.HandleFunc("/api/accounts/", s.management(s.accountAction))
	s.mux.HandleFunc("/api/images", s.management(s.images))
	s.mux.HandleFunc("/api/images/delete", s.management(s.deleteImages))
	s.mux.HandleFunc("/api/models", s.management(s.managementModels))
	s.mux.HandleFunc("/api/image-tasks", s.taskAuth(s.imageTasks))
	s.mux.HandleFunc("/api/image-tasks/", s.taskAuth(s.imageTaskAction))
	s.mux.HandleFunc("/api/logs", s.management(s.logs))
	s.mux.HandleFunc("/images/", s.imagesServe)
	s.mux.HandleFunc("/v1/models", s.client(s.models))
	s.mux.HandleFunc("/v1/images/generations", s.client(s.generateImage))
	s.mux.HandleFunc("/v1/images/edits", s.client(s.editImage))
	s.mux.HandleFunc("/", s.root)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Snapshot()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		allowedOrigin := origin != "" && cfg.AllowsOrigin(origin)
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if allowedOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Origin, Cache-Control, Pragma, X-Requested-With, X-API-Key, X-Management-Key")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			if origin != "" && !allowedOrigin {
				http.Error(w, "origin is not allowed", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if cfg.MaxRequestBodyBytes > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) management(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, enabled := s.auth.Management(r)
		if !enabled {
			http.NotFound(w, r)
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "management key is invalid")
			return
		}
		next(w, r)
	}
}

func (s *Server) client(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.APIKey(r); !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next(w, r)
	}
}

func (s *Server) taskAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.APIKey(r); ok {
			next(w, r)
			return
		}
		if ok, enabled := s.auth.Management(r); enabled && ok {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid API key")
	}
}

func (s *Server) identity(r *http.Request) string {
	if name, ok := s.auth.APIKey(r); ok {
		return name
	}
	return "admin"
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": "go-mvp"})
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": "go-mvp"})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	ok, enabled := s.auth.Management(r)
	if !enabled {
		http.NotFound(w, r)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "management key is invalid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": "go-mvp", "role": "admin", "subject_id": "admin", "name": "管理员",
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "chatgpt2api_accounts_total %d\n", stats.AccountsTotal)
	fmt.Fprintf(w, "chatgpt2api_accounts_active %d\n", stats.AccountsActive)
	fmt.Fprintf(w, "chatgpt2api_accounts_limited %d\n", stats.AccountsLimited)
	fmt.Fprintf(w, "chatgpt2api_jobs_queued %d\n", stats.JobsQueued)
	fmt.Fprintf(w, "chatgpt2api_jobs_running %d\n", stats.JobsRunning)
	fmt.Fprintf(w, "chatgpt2api_jobs_succeeded_total %d\n", stats.JobsSucceeded)
	fmt.Fprintf(w, "chatgpt2api_jobs_failed_total %d\n", stats.JobsFailed)
	fmt.Fprintf(w, "chatgpt2api_images_total %d\n", stats.ImagesTotal)
	fmt.Fprintf(w, "chatgpt2api_image_bytes %d\n", stats.ImageBytes)
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

func (s *Server) adminAsset(w http.ResponseWriter, r *http.Request) {
	web.Serve(w, r)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := s.cfg.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"stats":  stats,
		"config": map[string]any{
			"management":          cfg.ManagementEnabled(),
			"image_workers":       cfg.Performance.ImageWorkers,
			"parallel_generation": cfg.Performance.ParallelGeneration,
			"api_keys":            len(cfg.APIKeys),
		},
	})
}

func (s *Server) accessSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeAccessSettings(w)
	case http.MethodPut:
		var body struct {
			ManagementKey string  `json:"management_key"`
			Port          *int    `json:"port"`
			ProxyURL      *string `json:"proxy_url"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Port != nil && (*body.Port < 1 || *body.Port > 65535) {
			writeError(w, http.StatusBadRequest, "port must be between 1 and 65535")
			return
		}
		normalizedProxy := ""
		if body.ProxyURL != nil {
			normalizedProxy = strings.TrimSpace(*body.ProxyURL)
			if err := config.ValidateProxyURL(normalizedProxy); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if body.Port != nil || body.ProxyURL != nil {
			if err := s.cfg.Update(func(c *config.Config) {
				if body.Port != nil {
					c.Port = *body.Port
				}
				if body.ProxyURL != nil {
					c.ProxyURL = normalizedProxy
				}
			}); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if body.ProxyURL != nil && s.proxyUpdater != nil {
				s.proxyUpdater(normalizedProxy)
			}
		}
		if strings.TrimSpace(body.ManagementKey) != "" {
			if err := s.auth.UpdateManagementKey(body.ManagementKey); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		s.writeAccessSettings(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeAccessSettings(w http.ResponseWriter) {
	cfg := s.cfg.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"management_enabled":    cfg.ManagementEnabled(),
		"host":                  cfg.Host,
		"port":                  cfg.Port,
		"proxy_url":             cfg.ProxyURL,
		"auth_dir":              cfg.AuthDir,
		"data_dir":              cfg.DataDir,
		"sentinel_browser_path": cfg.Sentinel.BrowserPath,
		"sentinel_timezone":     cfg.Sentinel.Timezone,
	})
}

func (s *Server) apiKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.cfg.Snapshot()
		items := make([]map[string]any, 0, len(cfg.APIKeys))
		for _, item := range cfg.APIKeys {
			items = append(items, map[string]any{
				"name": item.Name, "masked": config.MaskAPIKey(item), "enabled": item.Enabled,
				"expires_at": item.ExpiresAt, "created_at": item.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = decodeJSON(r, &body)
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = "client"
		}
		raw, err := s.auth.CreateAPIKey(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": raw, "name": name})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) apiKeyByID(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/access/keys/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "key name is required")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.auth.DeleteAPIKey(name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPatch:
		var body struct {
			Enabled *bool  `json:"enabled"`
			Name    string `json:"name"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.auth.UpdateAPIKey(name, body.Enabled, body.Name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) accountsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.accounts.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
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
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.accounts.Import(r.Context(), body.Tokens, body.Accounts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		var body struct {
			IDs    []string `json:"ids"`
			Tokens []string `json:"tokens"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(body.IDs) == 0 && len(body.Tokens) > 0 {
			items, _ := s.accounts.List(r.Context())
			wanted := map[string]bool{}
			for _, t := range body.Tokens {
				wanted[t] = true
			}
			for _, item := range items {
				if wanted[item.AccessToken] {
					body.IDs = append(body.IDs, item.ID)
				}
			}
		}
		removed, err := s.accounts.Delete(r.Context(), body.IDs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) refreshAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	_ = decodeJSON(r, &body)
	result := s.accounts.RefreshMany(r.Context(), body.IDs)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) accountAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var updates map[string]any
		if err := decodeJSON(r, &updates); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		item, err := s.accounts.Update(r.Context(), id, updates)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"item": item.Public()})
		return
	}
	if len(parts) == 2 && parts[1] == "refresh" && r.Method == http.MethodPost {
		if err := s.accounts.RefreshOne(r.Context(), id); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	_ = decodeJSON(r, &body)
	items, err := s.accounts.Export(r.Context(), body.IDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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

func (s *Server) importAccountDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	_ = decodeJSON(r, &body)
	path := strings.TrimSpace(body.Path)
	cfg := s.cfg.Snapshot()
	if path == "" {
		path = cfg.AuthDirPath()
	} else {
		path = cfg.ResolvePath(path)
	}
	result, err := s.accounts.ImportDirectory(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) importAccountJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 20<<20+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read account JSON: "+err.Error())
		return
	}
	if len(data) > 20<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "account JSON is too large")
		return
	}
	items, err := account.ParseImportJSON(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.accounts.Import(r.Context(), nil, items)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	items, err := s.db.ListAllImages(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	origin := requestOrigin(r)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"rel_path": item.RelPath, "url": s.storage.SignedURL(origin, item.RelPath, 24*time.Hour),
			"width": item.Width, "height": item.Height, "bytes": item.Bytes, "created_at": item.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) deleteImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	removed := 0
	for _, rel := range body.Paths {
		item, err := s.db.GetImageByPath(r.Context(), rel)
		if err != nil {
			continue
		}
		if err := s.storage.Remove(item.RelPath); err != nil {
			continue
		}
		if err := s.db.DeleteImageRecord(r.Context(), item.ID); err == nil {
			removed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
}

func (s *Server) taskAuthOwner(r *http.Request) string {
	return s.identity(r)
}

func (s *Server) imageTasks(w http.ResponseWriter, r *http.Request) {
	owner := s.taskAuthOwner(r)
	if r.Method == http.MethodPost {
		var body struct {
			ClientTaskID string `json:"client_task_id"`
			Prompt       string `json:"prompt"`
			Model        string `json:"model"`
			Size         string `json:"size"`
			Quality      string `json:"quality"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(body.Prompt) == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		if body.Model == "" {
			body.Model = s.cfg.Snapshot().ImageGenerationModel
		}
		if body.Quality == "" {
			body.Quality = "auto"
		}
		job, _, err := s.db.CreateJob(r.Context(), store.Job{
			ClientTaskID: body.ClientTaskID, Owner: owner, Mode: "generate", Prompt: body.Prompt,
			Model: body.Model, Size: body.Size, Quality: body.Quality, MaxAttempts: 2,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.jobJSON(r, job, nil))
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var items []store.Job
	var err error
	if owner == "admin" {
		items, err = s.db.ListAllJobs(r.Context(), 100)
	} else {
		items, err = s.db.ListJobs(r.Context(), owner, 100)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		images, _ := s.db.ListImagesByJob(r.Context(), item.ID)
		out = append(out, s.jobJSON(r, item, images))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) imageTaskAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/image-tasks/")
	if path == "generations" && r.Method == http.MethodPost {
		var body struct {
			ClientTaskID string `json:"client_task_id"`
			Prompt       string `json:"prompt"`
			Model        string `json:"model"`
			Size         string `json:"size"`
			Quality      string `json:"quality"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(body.Prompt) == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		if body.Model == "" {
			body.Model = s.cfg.Snapshot().ImageGenerationModel
		}
		if body.Quality == "" {
			body.Quality = "auto"
		}
		job, _, err := s.db.CreateJob(r.Context(), store.Job{
			ClientTaskID: body.ClientTaskID, Owner: s.identity(r), Mode: "generate",
			Prompt: body.Prompt, Model: body.Model, Size: body.Size, Quality: body.Quality, MaxAttempts: 2,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.jobJSON(r, job, nil))
		return
	}
	if path == "edits" && r.Method == http.MethodPost {
		if err := r.ParseMultipartForm(25 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid multipart form")
			return
		}
		prompt := strings.TrimSpace(r.FormValue("prompt"))
		if prompt == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		files := r.MultipartForm.File["image"]
		if len(files) == 0 {
			files = r.MultipartForm.File["image[]"]
		}
		if len(files) == 0 {
			writeError(w, http.StatusBadRequest, "image is required")
			return
		}
		owner := s.identity(r)
		model := firstNonEmpty(r.FormValue("model"), s.cfg.Snapshot().ImageGenerationModel)
		quality := firstNonEmpty(r.FormValue("quality"), "auto")
		job, _, err := s.db.CreateJob(r.Context(), store.Job{
			ClientTaskID: r.FormValue("client_task_id"), Owner: owner, Mode: "edit",
			Prompt: prompt, Model: model, Size: r.FormValue("size"), Quality: quality, MaxAttempts: 2,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i, header := range files {
			data, err := readMultipartFile(header)
			if err != nil {
				_ = s.db.CancelJob(r.Context(), job.ID)
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			mime := header.Header.Get("Content-Type")
			rel, err := s.storage.SaveInput(r.Context(), job.ID, i+1, data, mime)
			if err != nil {
				_ = s.db.CancelJob(r.Context(), job.ID)
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := s.db.AddJobInput(r.Context(), store.JobInput{JobID: job.ID, Ordinal: i + 1, RelPath: rel, MIME: mime}); err != nil {
				_ = s.db.CancelJob(r.Context(), job.ID)
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, s.jobJSON(r, job, nil))
		return
	}
	if strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost {
		id := strings.TrimSuffix(path, "/cancel")
		if err := s.db.CancelJob(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   imageModelItems(),
	})
}

func (s *Server) managementModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":             "list",
		"items":              imageModelItems(),
		"default_model":      "gpt-image-2",
		"image_model":        "gpt-image-2",
		"upstream_available": true,
	})
}

func imageModelItems() []map[string]any {
	return []map[string]any{
		{
			"id": "gpt-image-2", "object": "model", "created": 0, "owned_by": "chatgpt2api",
			"permission": []any{}, "root": "gpt-image-2", "parent": nil, "title": "GPT Image 2",
		},
		{
			"id": "gpt-image-2.5", "object": "model", "created": 0, "owned_by": "chatgpt2api",
			"permission": []any{}, "root": "gpt-image-2.5", "parent": nil, "title": "GPT Image 2.5",
		},
	}
}

type imageRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	ResponseFormat string `json:"response_format"`
}

func (s *Server) generateImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body imageRequest
	if err := decodeJSON(r, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if body.N < 1 {
		body.N = 1
	}
	if body.N > 4 {
		body.N = 4
	}
	if body.Model == "" {
		body.Model = "gpt-image-2"
	}
	if body.Quality == "" {
		body.Quality = "auto"
	}
	jobs := make([]store.Job, 0, body.N)
	for i := 0; i < body.N; i++ {
		job, _, err := s.db.CreateJob(r.Context(), store.Job{
			Owner: s.identity(r), Mode: "generate", Prompt: body.Prompt, Model: body.Model,
			Size: body.Size, Quality: body.Quality, MaxAttempts: 2,
		})
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		done, err := s.waitJob(r.Context(), job.ID, 6*time.Minute)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, err.Error())
			return
		}
		jobs = append(jobs, done)
	}
	s.writeImageResponses(w, r, jobs, body.ResponseFormat)
}

func (s *Server) editImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	prompt := r.FormValue("prompt")
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	files := r.MultipartForm.File["image"]
	if len(files) == 0 {
		files = r.MultipartForm.File["image[]"]
	}
	if len(files) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "image is required")
		return
	}
	var images [][]byte
	for _, header := range files {
		data, err := readMultipartFile(header)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error())
			return
		}
		images = append(images, data)
	}
	model := r.FormValue("model")
	if model == "" {
		model = "gpt-image-2"
	}
	result, err := s.provider.Generate(r.Context(), provider.Request{
		Prompt: prompt, Model: model, Size: r.FormValue("size"), Quality: firstNonEmpty(r.FormValue("quality"), "auto"), Images: images,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	origin := requestOrigin(r)
	data := make([]map[string]any, 0, len(result.Images))
	for _, raw := range result.Images {
		img, err := s.storage.Save(r.Context(), s.identity(r), "edit-"+strconv.FormatInt(time.Now().UnixNano(), 10), raw, result.MIME)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.db.AddImage(r.Context(), img)
		item := map[string]any{"url": s.storage.SignedURL(origin, img.RelPath, 24*time.Hour), "revised_prompt": result.RevisedPrompt}
		if r.FormValue("response_format") == "b64_json" {
			item["b64_json"] = base64.StdEncoding.EncodeToString(raw)
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

func (s *Server) waitJob(ctx context.Context, id string, timeout time.Duration) (store.Job, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := s.db.GetJob(ctx, id)
		if err != nil {
			return job, err
		}
		switch job.Status {
		case store.JobSucceeded:
			return job, nil
		case store.JobFailed, store.JobCanceled:
			if job.Error == "" {
				job.Error = "image task failed"
			}
			return job, errors.New(job.Error)
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return store.Job{}, errors.New("image task timed out")
}

func (s *Server) writeImageResponse(w http.ResponseWriter, r *http.Request, job store.Job, responseFormat string) {
	s.writeImageResponses(w, r, []store.Job{job}, responseFormat)
}

func (s *Server) writeImageResponses(w http.ResponseWriter, r *http.Request, jobs []store.Job, responseFormat string) {
	var images []store.Image
	for _, job := range jobs {
		items, _ := s.db.ListImagesByJob(r.Context(), job.ID)
		images = append(images, items...)
	}
	origin := requestOrigin(r)
	data := make([]map[string]any, 0, len(images))
	for _, img := range images {
		prompt := ""
		for _, job := range jobs {
			if job.ID == img.JobID {
				prompt = job.Prompt
				break
			}
		}
		item := map[string]any{"url": s.storage.SignedURL(origin, img.RelPath, 24*time.Hour), "revised_prompt": prompt}
		if responseFormat == "b64_json" {
			if f, _, err := s.storage.Open(img.RelPath); err == nil {
				raw, _ := io.ReadAll(io.LimitReader(f, 32<<20))
				f.Close()
				item["b64_json"] = base64.StdEncoding.EncodeToString(raw)
			}
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

func (s *Server) jobJSON(r *http.Request, job store.Job, images []store.Image) map[string]any {
	origin := requestOrigin(r)
	imgs := make([]map[string]any, 0, len(images))
	for _, img := range images {
		imgs = append(imgs, map[string]any{"url": s.storage.SignedURL(origin, img.RelPath, 24*time.Hour), "width": img.Width, "height": img.Height, "bytes": img.Bytes})
	}
	return map[string]any{
		"id": job.ID, "status": job.Status, "mode": job.Mode, "model": job.Model, "size": job.Size,
		"quality": job.Quality, "created_at": job.CreatedAt, "updated_at": job.UpdatedAt,
		"progress": job.Progress, "error": job.Error, "images": imgs,
	}
}

func (s *Server) imagesServe(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/images/")
	if !s.storage.Verify(rel, r.URL.Query().Get("expires"), r.URL.Query().Get("sig")) {
		http.NotFound(w, r)
		return
	}
	f, mime, err := s.storage.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, f)
}

func (s *Server) clientError(w http.ResponseWriter, err error) {
	writeOpenAIError(w, http.StatusBadGateway, err.Error())
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return scheme + "://" + r.Host
}

func readMultipartFile(header *multipart.FileHeader) ([]byte, error) {
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 25<<20))
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 25<<20))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
