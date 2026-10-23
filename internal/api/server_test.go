package api

import (
	"bytes"
	"context"
	"encoding/json"
	stdimage "image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/auth"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/image"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
	"github.com/0fengzi0/chatgpt2api-go/internal/task"
)

func TestGenerationFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte("host: 127.0.0.1\nport: 0\ndata-dir: " + filepath.ToSlash(dir) + "\nimage:\n  max-image-pixels: 1000000\nperformance:\n  image-workers: 1\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, generated, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	management, apiKey := "", ""
	for _, item := range generated {
		if len(item) > 15 && item[:15] == "management-key=" {
			management = item[15:]
		}
		if len(item) > 8 && item[:8] == "api-key=" {
			apiKey = item[8:]
		}
	}
	cfgStore := config.NewStore(path, cfg)
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	storage, err := image.New(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	accounts := account.NewService(db, cfgStore)
	authManager := auth.New(cfgStore)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := task.NewWorker(db, cfgStore, storage, testImageProvider{}, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop()
	server := httptest.NewServer(New(cfgStore, db, accounts, authManager, storage, testImageProvider{}, logger).Handler())
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/images/generations", bytes.NewReader([]byte(`{"prompt":"test","size":"64x64","response_format":"url"}`)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("generation status %d: %s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	data, _ := result["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	statusReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/status", nil)
	statusReq.Header.Set("X-Management-Key", management)
	statusResp, err := http.DefaultClient.Do(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status returned %d", statusResp.StatusCode)
	}

	if _, err := db.AddAccount(context.Background(), store.Account{
		AccessToken: "json-import-token",
		Status:      store.AccountNormal,
	}); err != nil {
		t.Fatal(err)
	}
	importReq, _ := http.NewRequest(http.MethodPost, server.URL+"/api/accounts/import-json", bytes.NewReader([]byte(`{"data":{"items":[{"access_token":"json-import-token"}]}}`)))
	importReq.Header.Set("X-Management-Key", management)
	importReq.Header.Set("Content-Type", "application/json")
	importResp, err := http.DefaultClient.Do(importReq)
	if err != nil {
		t.Fatal(err)
	}
	defer importResp.Body.Close()
	if importResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(importResp.Body)
		t.Fatalf("JSON import status %d: %s", importResp.StatusCode, body)
	}
	var importResult map[string]any
	if err := json.NewDecoder(importResp.Body).Decode(&importResult); err != nil {
		t.Fatal(err)
	}
	if importResult["skipped"] != float64(1) {
		t.Fatalf("duplicate JSON import was not skipped: %#v", importResult)
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("prompt", "turn the square blue")
	_ = writer.WriteField("size", "64x64")
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	editReq, _ := http.NewRequest(http.MethodPost, server.URL+"/api/image-tasks/edits", &form)
	editReq.Header.Set("X-Management-Key", management)
	editReq.Header.Set("Content-Type", writer.FormDataContentType())
	editResp, err := http.DefaultClient.Do(editReq)
	if err != nil {
		t.Fatal(err)
	}
	defer editResp.Body.Close()
	if editResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(editResp.Body)
		t.Fatalf("edit status %d: %s", editResp.StatusCode, body)
	}
	var editJob map[string]any
	if err := json.NewDecoder(editResp.Body).Decode(&editJob); err != nil {
		t.Fatal(err)
	}
	editID, _ := editJob["id"].(string)
	if editID == "" {
		t.Fatal("edit job id is empty")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		taskReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/image-tasks", nil)
		taskReq.Header.Set("X-Management-Key", management)
		taskResp, err := http.DefaultClient.Do(taskReq)
		if err != nil {
			t.Fatal(err)
		}
		var list struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(taskResp.Body).Decode(&list)
		taskResp.Body.Close()
		for _, item := range list.Items {
			if item["id"] == editID && item["status"] == store.JobSucceeded {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("edit job did not succeed in time")
}

func TestAccessSettingsUpdatesProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	managementKey := "management-secret"
	hash, err := config.HashSecret(managementKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.RemoteManagement.SecretKey = hash
	cfgStore := config.NewStore(path, &cfg)
	updatedProxy := ""
	server := &Server{
		cfg:    cfgStore,
		auth:   auth.New(cfgStore),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		proxyUpdater: func(value string) {
			updatedProxy = value
		},
	}
	body := bytes.NewBufferString(`{"proxy_url":" http://127.0.0.1:10808 "}`)
	request := httptest.NewRequest(http.MethodPut, "/api/access/settings", body)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Management-Key", managementKey)
	response := httptest.NewRecorder()
	server.accessSettings(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if got := cfgStore.Snapshot().ProxyURL; got != "http://127.0.0.1:10808" {
		t.Fatalf("stored proxy = %q", got)
	}
	if updatedProxy != "http://127.0.0.1:10808" {
		t.Fatalf("proxy updater received %q", updatedProxy)
	}
}

func TestModelCatalogReturnsImageAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	managementKey := "management-secret"
	hash, err := config.HashSecret(managementKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.RemoteManagement.SecretKey = hash
	cfg.ImageGenerationModel = "gpt-image-2"
	cfg.DefaultUpstreamModel = "gpt-5-5"
	cfgStore := config.NewStore(path, &cfg)
	server := &Server{
		cfg:    cfgStore,
		auth:   auth.New(cfgStore),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	request.Header.Set("X-Management-Key", managementKey)
	response := httptest.NewRecorder()
	server.management(server.managementModels)(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Items             []map[string]any `json:"items"`
		DefaultModel      string           `json:"default_model"`
		UpstreamAvailable bool             `json:"upstream_available"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.UpstreamAvailable || len(result.Items) != 2 || result.DefaultModel != "gpt-image-2" {
		t.Fatalf("unexpected model catalog: %#v", result)
	}
	if result.Items[0]["id"] != "gpt-image-2" || result.Items[1]["id"] != "gpt-image-2.5" {
		t.Fatalf("unexpected image models: %#v", result.Items)
	}
}

func TestCORSMiddlewareAllowlist(t *testing.T) {
	cfg := config.Default()
	cfgStore := config.NewStore(filepath.Join(t.TempDir(), "config.yaml"), &cfg)
	server := &Server{cfg: cfgStore}
	handler := server.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodOptions, "/v1/images/generations", nil)
	request.Header.Set("Origin", "https://api.ai.isancha.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight status = %d", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://api.ai.isancha.com" {
		t.Fatalf("allow origin = %q", got)
	}
	if !strings.Contains(response.Header().Get("Vary"), "Origin") {
		t.Fatalf("missing Vary: Origin")
	}

	request = httptest.NewRequest(http.MethodOptions, "/v1/images/generations", nil)
	request.Header.Set("Origin", "https://example.com")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("rejected preflight status = %d", response.Code)
	}
}

type testImageProvider struct{}

func (testImageProvider) Generate(_ context.Context, req provider.Request) (provider.Result, error) {
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return provider.Result{}, err
	}
	if req.Progress != nil {
		req.Progress("test_image_ready")
	}
	return provider.Result{
		Images:        [][]byte{buf.Bytes()},
		MIME:          "image/png",
		RevisedPrompt: req.Prompt,
	}, nil
}
