package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/api"
	"github.com/0fengzi0/chatgpt2api-go/internal/auth"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/image"
	"github.com/0fengzi0/chatgpt2api-go/internal/maintenance"
	"github.com/0fengzi0/chatgpt2api-go/internal/migrate"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/sentinelbrowser"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
	"github.com/0fengzi0/chatgpt2api-go/internal/task"
)

func main() {
	configPath := flag.String("config", config.DefaultConfigPath, "path to config.yaml or config.json")
	migrateLegacy := flag.Bool("migrate-legacy", false, "import data/accounts.json and data/auth_keys.json from the Python version")
	setManagementKey := flag.String("set-management-key", "", "rotate the management key and exit after saving")
	cleanupImages := flag.Bool("cleanup-images", false, "delete expired images once and exit")
	flag.Parse()

	cfg, generated, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	logger := newLogger(cfg)
	if err := config.ValidateProxyURL(cfg.ProxyURL); err != nil {
		logger.Error("validate proxy configuration", "error", err)
		os.Exit(1)
	}
	if len(generated) > 0 {
		fmt.Fprintln(os.Stdout, "First-start credentials generated. Store them now; they will not be shown again:")
		for _, item := range generated {
			fmt.Fprintln(os.Stdout, "  "+item)
		}
	}
	cfgStore := config.NewStore(*configPath, cfg)
	if strings.TrimSpace(*setManagementKey) != "" {
		hash, err := config.HashSecret(*setManagementKey)
		if err != nil {
			logger.Error("hash management key", "error", err)
			os.Exit(1)
		}
		if err := cfgStore.Update(func(c *config.Config) { c.RemoteManagement.SecretKey = hash }); err != nil {
			logger.Error("save management key", "error", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "management key updated")
		return
	}

	db, err := store.Open(cfg.DBPath(), cfg.SigningKey)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	storage, err := image.New(*cfg)
	if err != nil {
		logger.Error("open image storage", "error", err)
		os.Exit(1)
	}
	if *cleanupImages {
		removed, err := maintenance.New(db, cfgStore, storage, logger).RunOnce(context.Background())
		if err != nil {
			logger.Error("cleanup images", "error", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "cleaned images: %d\n", removed)
		return
	}
	if recovered, err := db.RecoverRunningJobs(context.Background()); err != nil {
		logger.Warn("recover interrupted jobs", "error", err)
	} else if recovered > 0 {
		logger.Info("recovered interrupted image jobs", "count", recovered)
	}
	if *migrateLegacy {
		if count, err := migrate.LegacyAccountFiles(context.Background(), db, cfg.DataDirPath()); err != nil {
			logger.Warn("migrate legacy accounts", "error", err)
		} else if count > 0 {
			logger.Info("imported legacy accounts", "count", count)
		}
		if count, err := migrate.LegacyAuthKeys(filepath.Join(cfg.DataDirPath(), "auth_keys.json"), cfgStore); err != nil {
			logger.Warn("migrate legacy auth keys", "error", err)
		} else if count > 0 {
			logger.Info("imported legacy API keys", "count", count)
		}
	}

	accounts := account.NewService(db, cfgStore)
	authManager := auth.New(cfgStore)
	solver := sentinelbrowser.New(sentinelbrowser.Options{
		BrowserPath:     cfg.Sentinel.BrowserPath,
		ProxyURL:        cfg.ProxyURL,
		Timezone:        cfg.Sentinel.Timezone,
		Headless:        cfg.Sentinel.Headless,
		NoSandbox:       cfg.Sentinel.NoSandbox,
		RefreshInterval: time.Duration(cfg.Sentinel.RefreshMinutes) * time.Minute,
		NavigateTimeout: time.Duration(cfg.Sentinel.NavigateTimeoutSecs) * time.Second,
		SolveTimeout:    time.Duration(cfg.Sentinel.SolveTimeoutSeconds) * time.Second,
	})
	defer solver.Close()
	imageProvider := provider.NewChatGPT(accounts, cfgStore, solver)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		startupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := solver.Start(startupCtx); err != nil && ctx.Err() == nil {
			logger.Warn("embedded sentinel solver prewarm failed", "error", err)
		} else if err == nil {
			status := solver.Status()
			logger.Info("embedded sentinel solver ready", "browser", status.BrowserVersion, "path", status.BrowserPath)
		}
	}()
	worker := task.NewWorker(db, cfgStore, storage, imageProvider, logger)
	worker.Start(ctx)
	defer worker.Stop()
	go accountRefreshLoop(ctx, accounts, cfgStore, logger)
	maintenance.New(db, cfgStore, storage, logger).Start(ctx)

	server := &http.Server{
		Addr: listenAddress(cfg.Host, cfg.Port),
		Handler: api.New(
			cfgStore, db, accounts, authManager, storage, imageProvider, logger,
			api.WithProxyUpdater(solver.SetProxyURL),
		).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		logger.Info("chatgpt2api listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Info("shutdown complete")
}

func listenAddress(host string, port int) string {
	if host == "" {
		return ":" + strconv.Itoa(port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	var writer io.Writer = os.Stdout
	if cfg.LoggingToFile {
		_ = os.MkdirAll(cfg.DataDirPath(), 0o755)
		if f, err := os.OpenFile(filepath.Join(cfg.DataDirPath(), "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			writer = io.MultiWriter(os.Stdout, f)
		}
	}
	return slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level}))
}

func accountRefreshLoop(ctx context.Context, accounts *account.Service, cfgStore *config.Store, logger *slog.Logger) {
	interval := time.Duration(cfgStore.Snapshot().RefreshIntervalMinute) * time.Minute
	if interval < time.Minute {
		interval = 30 * time.Minute
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if len(accountsMustList(ctx, accounts)) > 0 {
				result := accounts.RefreshMany(ctx, nil)
				logger.Info("scheduled account refresh", "refreshed", result.Refreshed, "errors", len(result.Errors))
			}
			timer.Reset(interval)
		}
	}
}

func accountsMustList(ctx context.Context, accounts *account.Service) []store.Account {
	items, err := accounts.List(ctx)
	if err != nil {
		return nil
	}
	return items
}
