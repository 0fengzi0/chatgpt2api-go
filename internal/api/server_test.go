package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/auth"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/limiter"
	"github.com/0fengzi0/chatgpt2api-go/internal/output"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

type testServer struct {
	server     *Server
	httpServer *httptest.Server
	cfgStore   *config.Store
	db         *store.Store
	apiKey     string
	management string
}

func newTestServer(t *testing.T) *testServer {
	return newTestServerWithProvider(t, testImageProvider{})
}

func newTestServerWithProvider(t *testing.T, imageProvider provider.Provider) *testServer {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte("host: 127.0.0.1\nport: 0\ndata-dir: " + filepath.ToSlash(dir) + "\n" +
		"performance:\n  image-workers: 2\nimage:\n  max-image-pixels: 16777216\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, generated, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	management, apiKey := "", ""
	for _, item := range generated {
		if strings.HasPrefix(item, "management-key=") {
			management = strings.TrimPrefix(item, "management-key=")
		}
		if strings.HasPrefix(item, "api-key=") {
			apiKey = strings.TrimPrefix(item, "api-key=")
		}
	}
	cfgStore := config.NewStore(path, cfg)
	db, err := store.Open(cfg.DBPath(), cfg.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	outputs, err := output.New(cfgStore, db)
	if err != nil {
		t.Fatal(err)
	}
	accounts := account.NewService(db, cfgStore)
	authManager := auth.New(cfgStore)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(
		cfgStore, db, accounts, authManager, outputs, imageProvider,
		limiter.New(cfgStore), logger,
	)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &testServer{
		server: server, httpServer: httpServer, cfgStore: cfgStore, db: db,
		apiKey: apiKey, management: management,
	}
}

func (s *testServer) request(t *testing.T, method, path string, body io.Reader, apiKey bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.httpServer.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGenerationBase64AndURL(t *testing.T) {
	ts := newTestServer(t)
	for _, responseFormat := range []string{"b64_json", "url"} {
		body := `{"model":"gpt-image-2","prompt":"test image","size":"64x64","response_format":"` + responseFormat + `"}`
		resp := ts.request(t, http.MethodPost, "/v1/images/generations", strings.NewReader(body), true)
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s status %d: %s", responseFormat, resp.StatusCode, raw)
		}
		var result struct {
			Data []map[string]any `json:"data"`
			Size string           `json:"size"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(result.Data) != 1 || result.Size != "8x8" {
			t.Fatalf("unexpected response: %#v", result)
		}
		if responseFormat == "b64_json" {
			encoded, _ := result.Data[0]["b64_json"].(string)
			if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
				t.Fatalf("invalid base64: %v", err)
			}
		} else {
			url, _ := result.Data[0]["url"].(string)
			if !strings.Contains(url, "/generated/") {
				t.Fatalf("unexpected temporary URL: %q", url)
			}
			download, err := http.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			download.Body.Close()
			if download.StatusCode != http.StatusOK {
				t.Fatalf("temporary URL status = %d", download.StatusCode)
			}
		}
	}
}

func TestEditFlow(t *testing.T) {
	ts := newTestServer(t)
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "make it blue")
	_ = writer.WriteField("size", "64x64")
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(testPNG()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.httpServer.URL+"/v1/images/edits", &form)
	req.Header.Set("Authorization", "Bearer "+ts.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("edit status %d: %s", resp.StatusCode, raw)
	}
}

func TestEditFlowWithMaskAndWebP(t *testing.T) {
	ts := newTestServer(t)
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "replace the masked area")
	_ = writer.WriteField("output_format", "webp")
	imagePart, _ := writer.CreateFormFile("image[]", "input.png")
	_, _ = imagePart.Write(testPNG())
	maskPart, _ := writer.CreateFormFile("mask", "mask.png")
	_, _ = maskPart.Write(testPNG())
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.httpServer.URL+"/v1/images/edits", &form)
	req.Header.Set("Authorization", "Bearer "+ts.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("masked edit status %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OutputFormat string           `json:"output_format"`
		Data         []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.OutputFormat != "webp" || len(result.Data) != 1 {
		t.Fatalf("unexpected masked edit result: %#v", result)
	}
	encoded, _ := result.Data[0]["b64_json"].(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, format, err := image.Decode(bytes.NewReader(raw)); err != nil || format != "webp" {
		t.Fatalf("decoded format=%q err=%v", format, err)
	}
}

func TestModelsAndStrictValidation(t *testing.T) {
	ts := newTestServer(t)
	resp := ts.request(t, http.MethodGet, "/v1/models", nil, true)
	var models struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(models.Data) != 4 {
		t.Fatalf("model count = %d", len(models.Data))
	}
	resp = ts.request(t, http.MethodGet, "/v1/models/gpt-image-2.5-flare", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model detail status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = ts.request(t, http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"test","unknown":true}`), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown parameter status = %d", resp.StatusCode)
	}
	var result struct {
		Error map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if result.Error["code"] != "unsupported_parameter" {
		t.Fatalf("unexpected error: %#v", result.Error)
	}

	resp = ts.request(t, http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"test","n":5}`), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("n validation status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIKeyDisabledAndCORS(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.server.auth.UpdateAPIKey("default", boolPointer(false), ""); err != nil {
		t.Fatal(err)
	}
	resp := ts.request(t, http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"test"}`), true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled key status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	request := httptest.NewRequest(http.MethodOptions, "/v1/images/generations", nil)
	request.Header.Set("Origin", "https://api.ai.isancha.com")
	response := httptest.NewRecorder()
	ts.server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight = %d", response.Code)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://api.ai.isancha.com" {
		t.Fatalf("missing allow origin")
	}
}

func TestAccessSettingsUpdate(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPut, ts.httpServer.URL+"/api/access/settings",
		strings.NewReader(`{"global_concurrency":5,"account_concurrency":2,"request_timeout_seconds":240,"temporary_url_ttl_seconds":900,"log_retention_days":14,"proxy_url":"http://127.0.0.1:10808"}`))
	req.Header.Set("X-Management-Key", ts.management)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("settings status %d: %s", resp.StatusCode, raw)
	}
	cfg := ts.cfgStore.Snapshot()
	if cfg.Performance.ImageWorkers != 5 || cfg.Image.AccountConcurrency != 2 ||
		cfg.Image.RequestTimeoutSeconds != 240 || cfg.Image.TemporaryURLTTLSeconds != 900 ||
		cfg.Image.LogRetentionDays != 14 {
		t.Fatalf("settings not saved: %#v", cfg)
	}
}

func TestRequestLogIsWritten(t *testing.T) {
	ts := newTestServer(t)
	resp := ts.request(t, http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"logged"}`), true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generation status = %d", resp.StatusCode)
	}
	items, err := ts.db.ListRequestLogs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Owner != "default" || items[0].Model != "gpt-image-2" {
		t.Fatalf("unexpected request log: %#v", items)
	}
}

func TestStreamingImageResponse(t *testing.T) {
	ts := newTestServer(t)
	body := `{"model":"gpt-image-2.5-flare","prompt":"stream test","stream":true,"partial_images":2,"output_format":"webp"}`
	resp := ts.request(t, http.MethodPost, "/v1/images/generations", strings.NewReader(body), true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status %d: %s", resp.StatusCode, raw)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "event: image_generation.completed") ||
		!strings.Contains(text, `"output_format":"webp"`) {
		t.Fatalf("unexpected stream: %s", text)
	}
}

func TestPartialMultiImageFailureIsLogged(t *testing.T) {
	ts := newTestServerWithProvider(t, &partialFailureProvider{})
	resp := ts.request(t, http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"partial","n":2}`), true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("partial failure status = %d", resp.StatusCode)
	}
	items, err := ts.db.ListRequestLogs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SucceededImages != 1 || items[0].QuotaUsed != 1 {
		t.Fatalf("partial failure log = %#v", items)
	}
}

type testImageProvider struct{}

func (testImageProvider) Generate(_ context.Context, req provider.Request) (provider.Result, error) {
	return provider.Result{
		Images:        [][]byte{testPNG()},
		MIME:          "image/png",
		RevisedPrompt: req.Prompt,
		AccountID:     "test-account",
	}, nil
}

type partialFailureProvider struct {
	calls atomic.Int64
}

func (p *partialFailureProvider) Generate(ctx context.Context, req provider.Request) (provider.Result, error) {
	call := p.calls.Add(1)
	if call > 1 {
		return provider.Result{}, errors.New("test upstream failure")
	}
	select {
	case <-ctx.Done():
	case <-time.After(50 * time.Millisecond):
	}
	return provider.Result{
		Images: [][]byte{testPNG()}, MIME: "image/png", RevisedPrompt: req.Prompt,
		AccountID: "partial-success",
	}, nil
}

func testPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

func boolPointer(value bool) *bool {
	return &value
}
