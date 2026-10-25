package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/auth"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/limiter"
	"github.com/0fengzi0/chatgpt2api-go/internal/output"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
	"github.com/0fengzi0/chatgpt2api-go/internal/web"
)

type Server struct {
	cfg          *config.Store
	db           *store.Store
	accounts     *account.Service
	auth         *auth.Manager
	outputs      *output.Storage
	provider     provider.Provider
	limiter      *limiter.Manager
	logger       *slog.Logger
	mux          *http.ServeMux
	proxyUpdater func(string)
	readyCheck   func() bool
	solverStatus func() any
	startedAt    time.Time
	requests     atomic.Int64
	errors       atomic.Int64
}

type Option func(*Server)

func WithProxyUpdater(update func(string)) Option {
	return func(server *Server) {
		server.proxyUpdater = update
	}
}

func WithReadiness(check func() bool) Option {
	return func(server *Server) {
		server.readyCheck = check
	}
}

func WithSolverStatus(status func() any) Option {
	return func(server *Server) {
		server.solverStatus = status
	}
}

func New(
	cfg *config.Store,
	db *store.Store,
	accounts *account.Service,
	authManager *auth.Manager,
	outputs *output.Storage,
	p provider.Provider,
	requestLimiter *limiter.Manager,
	logger *slog.Logger,
	options ...Option,
) *Server {
	server := &Server{
		cfg:       cfg,
		db:        db,
		accounts:  accounts,
		auth:      authManager,
		outputs:   outputs,
		provider:  p,
		limiter:   requestLimiter,
		logger:    logger,
		mux:       http.NewServeMux(),
		startedAt: time.Now().UTC(),
	}
	for _, option := range options {
		option(server)
	}
	server.routes()
	return server
}

func (s *Server) Handler() http.Handler {
	return s.middleware(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/readyz", s.ready)
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
	s.mux.HandleFunc("/api/accounts/import-json", s.management(s.importAccountJSON))
	s.mux.HandleFunc("/api/accounts/", s.management(s.accountAction))
	s.mux.HandleFunc("/api/logs", s.management(s.logs))
	s.mux.HandleFunc("/api/logs/delete", s.management(s.deleteLogs))
	s.mux.HandleFunc("/api/account-events", s.management(s.accountEvents))

	s.mux.HandleFunc("/generated/", s.generatedFile)
	s.mux.HandleFunc("/v1/models", s.client(s.models))
	s.mux.HandleFunc("/v1/models/", s.client(s.modelByID))
	s.mux.HandleFunc("/v1/images/generations", s.client(s.generateImage))
	s.mux.HandleFunc("/v1/images/edits", s.client(s.editImage))
	s.mux.HandleFunc("/", s.root)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Snapshot()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")

		origin := strings.TrimSpace(r.Header.Get("Origin"))
		allowedOrigin := origin != "" && cfg.AllowsOrigin(origin)
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if allowedOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Origin, Cache-Control, Pragma, X-Requested-With, X-API-Key, X-Management-Key, X-Request-ID")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Content-Disposition")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			if origin != "" && !allowedOrigin {
				writeOpenAIError(w, openAIError{
					Status: http.StatusForbidden, Type: "invalid_request_error",
					Code: "origin_not_allowed", Message: "origin is not allowed",
				})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if cfg.MaxRequestBodyBytes > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxRequestBodyBytes)
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		s.requests.Add(1)
		next.ServeHTTP(w, r.WithContext(ctx))
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
			writeManagementError(w, http.StatusUnauthorized, "management key is invalid")
			return
		}
		next(w, r)
	}
}

func (s *Server) client(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyName, err := s.auth.AuthenticateAPIKey(r)
		if err != nil {
			switch err {
			case auth.ErrDisabledAPIKey:
				writeOpenAIError(w, openAIError{
					Status: http.StatusForbidden, Type: "invalid_request_error",
					Code: "api_key_disabled", Message: "API key is disabled",
				})
			case auth.ErrMissingAPIKey, auth.ErrInvalidAPIKey:
				writeOpenAIError(w, openAIError{
					Status: http.StatusUnauthorized, Type: "invalid_request_error",
					Code: "invalid_api_key", Message: "invalid API key",
				})
			default:
				writeOpenAIError(w, openAIError{
					Status: http.StatusUnauthorized, Type: "invalid_request_error",
					Code: "invalid_api_key", Message: "invalid API key",
				})
			}
			return
		}
		ctx := context.WithValue(r.Context(), ownerKey{}, keyName)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": "image-gateway",
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reasons := make([]string, 0)
	if err := s.db.Ping(r.Context()); err != nil {
		reasons = append(reasons, "database unavailable")
	}
	rows, err := s.accounts.List(r.Context())
	if err != nil {
		reasons = append(reasons, "accounts unavailable")
	} else {
		usable := false
		for _, item := range rows {
			if item.Status == store.AccountNormal || item.Status == store.AccountLimited {
				usable = true
				break
			}
		}
		if !usable {
			reasons = append(reasons, "no usable account")
		}
	}
	solverReady := s.readyCheck == nil || s.readyCheck()
	if !solverReady {
		reasons = append(reasons, "sentinel solver not ready")
	}
	status := "ok"
	code := http.StatusOK
	if len(reasons) > 0 {
		status = "not_ready"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":  status,
		"reasons": reasons,
		"uptime":  int64(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	active, waiting := s.limiter.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writeMetric(w, "chatgpt2api_requests_total", float64(stats.RequestsTotal))
	writeMetric(w, "chatgpt2api_requests_failed_total", float64(stats.RequestsFailed))
	writeMetric(w, "chatgpt2api_request_duration_ms_avg", stats.RequestDurationAvgMS)
	writeMetric(w, "chatgpt2api_requests_today", float64(stats.RequestsToday))
	writeMetric(w, "chatgpt2api_requests_failed_today", float64(stats.RequestsFailedToday))
	writeMetric(w, "chatgpt2api_images_today", float64(stats.ImagesToday))
	writeMetric(w, "chatgpt2api_request_duration_ms_avg_today", stats.RequestDurationAvgTodayMS)
	writeMetric(w, "chatgpt2api_requests_inflight", float64(active))
	writeMetric(w, "chatgpt2api_requests_queued", float64(waiting))
	writeMetric(w, "chatgpt2api_accounts_total", float64(stats.AccountsTotal))
	writeMetric(w, "chatgpt2api_accounts_active", float64(stats.AccountsActive))
	writeMetric(w, "chatgpt2api_accounts_limited", float64(stats.AccountsLimited))
	writeMetric(w, "chatgpt2api_accounts_error", float64(stats.AccountsError))
	writeMetric(w, "chatgpt2api_accounts_disabled", float64(stats.AccountsDisabled))
	writeMetric(w, "chatgpt2api_account_quota_remaining", float64(stats.QuotaTotal))
	writeMetric(w, "chatgpt2api_temporary_files", float64(stats.TemporaryFiles))
	writeMetric(w, "chatgpt2api_temporary_bytes", float64(stats.TemporaryBytes))
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": "image-gateway"})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ok, enabled := s.auth.Management(r)
	if !enabled {
		http.NotFound(w, r)
		return
	}
	if !ok {
		writeManagementError(w, http.StatusUnauthorized, "management key is invalid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": "image-gateway", "role": "admin",
		"subject_id": "admin", "name": "Administrator",
	})
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

func (s *Server) generatedFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/generated/")
	extension := ""
	for _, candidate := range []string{".png", ".jpg", ".webp"} {
		if strings.HasSuffix(strings.ToLower(name), candidate) {
			extension = candidate
			name = name[:len(name)-len(candidate)]
			break
		}
	}
	if name == "" || extension == "" || !s.outputs.Verify(name, r.URL.Query().Get("expires"), r.URL.Query().Get("sig")) {
		http.NotFound(w, r)
		return
	}
	item, err := s.db.GetTemporaryFile(r.Context(), name)
	if err != nil || item.ExpiresAt < time.Now().UTC().Format(time.RFC3339) {
		http.NotFound(w, r)
		return
	}
	file, err := s.outputs.Open(item.RelPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", item.MIME)
	w.Header().Set("Content-Disposition", `inline; filename="image`+extension+`"`)
	w.Header().Set("Cache-Control", "private, max-age=600")
	modTime, _ := time.Parse(time.RFC3339, item.CreatedAt)
	if modTime.IsZero() {
		modTime = time.Now()
	}
	http.ServeContent(w, r, "image"+extension, modTime, file)
}

func writeMetric(w http.ResponseWriter, name string, value float64) {
	_, _ = w.Write([]byte(name + " " + strconv.FormatFloat(value, 'f', -1, 64) + "\n"))
}

func newRequestID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "req-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "req-" + hex.EncodeToString(buffer)
}

type requestIDKey struct{}
type ownerKey struct{}
type safetyIdentifierKey struct{}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func owner(ctx context.Context) string {
	value, _ := ctx.Value(ownerKey{}).(string)
	return value
}
