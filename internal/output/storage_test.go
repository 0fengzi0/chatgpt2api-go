package output

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

func TestTemporaryImageSignatureAndOpen(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.SigningKey = "test-signing-key"
	cfg.PublicBaseURL = "https://gateway.example.com"
	cfgStore := config.NewStore(filepath.Join(dir, "config.yaml"), &cfg)
	db, err := store.Open(cfg.DBPath(), cfg.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	storage, err := New(cfgStore, db)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	item, err := storage.Save(context.Background(), buffer.Bytes(), "image/png", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	signed := storage.SignedURL("http://127.0.0.1:3000", item)
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "gateway.example.com" || parsed.Query().Get("sig") == "" {
		t.Fatalf("unexpected signed URL: %s", signed)
	}
	if !storage.Verify(item.ID, parsed.Query().Get("expires"), parsed.Query().Get("sig")) {
		t.Fatal("signature did not verify")
	}
	file, err := storage.Open(item.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestCleanupExpiredLogs(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.SigningKey = "test-signing-key"
	cfg.Image.LogRetentionDays = 30
	cfgStore := config.NewStore(filepath.Join(dir, "config.yaml"), &cfg)
	db, err := store.Open(cfg.DBPath(), cfg.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	storage, err := New(cfgStore, db)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := db.AddRequestLog(context.Background(), store.RequestLog{
		RequestID: "old", Owner: "default", Status: 200, CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	removed, err := storage.CleanupLogs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed logs = %d", removed)
	}
}
