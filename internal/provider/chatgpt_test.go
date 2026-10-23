package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

func TestChatGPTGenerateWithMockUpstream(t *testing.T) {
	var imageBytes bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&imageBytes, img); err != nil {
		t.Fatal(err)
	}
	const expectedTurnstileToken = "solver-turnstile-token"
	finalizeTokens := make(chan string, 1)
	solver := &testTurnstileSolver{
		userAgent: "SolverBrowser/1.0",
		token:     expectedTurnstileToken,
		requests:  make(chan turnstileCall, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html data-build="test"><script src="/sdk.js"></script></html>`))
	})
	mux.HandleFunc("/backend-api/sentinel/chat-requirements/prepare", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"prepare_token": "prep",
			"proofofwork":   map[string]any{"required": false},
			"turnstile":     map[string]any{"required": true, "dx": "challenge-dx"},
		})
	})
	mux.HandleFunc("/backend-api/sentinel/chat-requirements/finalize", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		finalizeTokens <- stringValue(body["turnstile_token"])
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "requirements-token"})
	})
	mux.HandleFunc("/backend-api/f/conversation/prepare", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"conduit_token": "conduit"})
	})
	mux.HandleFunc("/backend-api/f/conversation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"conversation_id\":\"conv-1\",\"message\":{\"content\":{\"parts\":[{\"asset_pointer\":\"file-service://file_123\"}]}}}\n\ndata: [DONE]\n\n"))
	})
	mux.HandleFunc("/backend-api/files/file_123/download", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"download_url": "http://" + r.Host + "/image"})
	})
	mux.HandleFunc("/image", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageBytes.Bytes())
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	configText := "provider-base-url: " + upstream.URL +
		"\ndata-dir: " + filepath.ToSlash(dir) + "\n"
	if err := os.WriteFile(path, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfgStore := config.NewStore(path, cfg)
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.AddAccount(context.Background(), store.Account{
		AccessToken: "token", Type: "Plus", SourceType: "web", Status: store.AccountNormal, Quota: 1,
	}); err != nil {
		t.Fatal(err)
	}
	accounts := account.NewService(db, cfgStore)
	p := NewChatGPT(accounts, cfgStore, solver)
	result, err := p.Generate(context.Background(), Request{Prompt: "test", Model: "gpt-image-2", Size: "2x2"})
	if err != nil {
		t.Fatal(err)
	}
	call := <-solver.requests
	if call.dx != "challenge-dx" || call.p == "" {
		t.Fatalf("unexpected solver call: %#v", call)
	}
	if call.requirements["prepare_token"] != "prep" {
		t.Fatalf("solver did not receive prepare response: %#v", call.requirements)
	}
	if got := <-finalizeTokens; got != expectedTurnstileToken {
		t.Fatalf("finalize turnstile token = %q, want %q", got, expectedTurnstileToken)
	}
	if len(result.Images) != 1 || len(result.Images[0]) == 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestResolveImageModel(t *testing.T) {
	for _, requestModel := range []string{"", "gpt-image-2", "gpt-image-2.5"} {
		got, err := resolveImageModel(requestModel, "gpt-5-5")
		if err != nil {
			t.Fatalf("resolveImageModel(%q): %v", requestModel, err)
		}
		if got != "gpt-5-5" {
			t.Fatalf("resolveImageModel(%q) = %q", requestModel, got)
		}
	}
	if _, err := resolveImageModel("gpt-6", ""); err == nil {
		t.Fatal("expected non-image model rejection")
	}
}

type turnstileCall struct {
	dx           string
	p            string
	requirements map[string]any
}

type testTurnstileSolver struct {
	userAgent string
	token     string
	requests  chan turnstileCall
}

func (solver *testTurnstileSolver) UserAgent(context.Context) (string, error) {
	return solver.userAgent, nil
}

func (solver *testTurnstileSolver) SolveTurnstile(_ context.Context, dx, p string, requirements map[string]any) (string, error) {
	solver.requests <- turnstileCall{dx: dx, p: p, requirements: requirements}
	return solver.token, nil
}
