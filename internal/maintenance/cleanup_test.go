package maintenance

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/image"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

func TestRunOnceRemovesExpiredImagesAndRecords(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.SigningKey = "cleanup-test-signing-key"
	cfg.Image.RetentionDays = 7
	cfgStore := config.NewStore(filepath.Join(dir, "config.yaml"), &cfg)

	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	storage, err := image.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339)

	expired, err := storage.Save(ctx, "admin", "expired-job", []byte("expired-image"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	expired.CreatedAt = old
	if err := db.AddImage(ctx, expired); err != nil {
		t.Fatal(err)
	}
	missing := store.Image{
		JobID: "missing-job", Owner: "admin", RelPath: "missing/file.png",
		MIME: "image/png", Bytes: 10, CreatedAt: old,
	}
	if err := db.AddImage(ctx, missing); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	removed, err := New(db, cfgStore, storage, logger).RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, err := os.Stat(filepath.Join(cfg.ImageDir(), filepath.FromSlash(expired.RelPath))); !os.IsNotExist(err) {
		t.Fatalf("expired image file still exists: %v", err)
	}
	remaining, err := db.ListAllImages(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("image records remain: %#v", remaining)
	}
}
