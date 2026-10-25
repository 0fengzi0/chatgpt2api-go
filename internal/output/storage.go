package output

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

type Storage struct {
	config *config.Store
	db     *store.Store
	root   string
}

func New(cfg *config.Store, db *store.Store) (*Storage, error) {
	snapshot := cfg.Snapshot()
	root := filepath.Join(snapshot.DataDirPath(), "temporary-images")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Storage{config: cfg, db: db, root: root}, nil
}

func (s *Storage) Save(ctx context.Context, data []byte, mime string, width, height int) (store.TemporaryFile, error) {
	if len(data) == 0 {
		return store.TemporaryFile{}, fmt.Errorf("empty image")
	}
	cfg := s.config.Snapshot()
	if cfg.Image.MaxImageBytes > 0 && int64(len(data)) > cfg.Image.MaxImageBytes {
		return store.TemporaryFile{}, fmt.Errorf("image exceeds max bytes")
	}
	id, err := randomID()
	if err != nil {
		return store.TemporaryFile{}, err
	}
	extension := ExtensionForMIME(mime)
	rel := id + extension
	full := filepath.Join(s.root, rel)
	if err := os.WriteFile(full, data, 0o600); err != nil {
		return store.TemporaryFile{}, err
	}
	nowValue := time.Now().UTC()
	item := store.TemporaryFile{
		ID:        id,
		RelPath:   rel,
		MIME:      mime,
		Width:     width,
		Height:    height,
		Bytes:     int64(len(data)),
		ExpiresAt: nowValue.Add(time.Duration(cfg.Image.TemporaryURLTTLSeconds) * time.Second).Format(time.RFC3339),
		CreatedAt: nowValue.Format(time.RFC3339),
	}
	if err := s.db.AddTemporaryFile(ctx, item); err != nil {
		_ = os.Remove(full)
		return store.TemporaryFile{}, err
	}
	return item, nil
}

func (s *Storage) Open(rel string) (*os.File, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

func (s *Storage) Remove(rel string) error {
	full, err := s.resolve(rel)
	if err != nil {
		return err
	}
	err = os.Remove(full)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Storage) SignedURL(origin string, item store.TemporaryFile) string {
	cfg := s.config.Snapshot()
	base := strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if base == "" {
		base = strings.TrimRight(origin, "/")
	}
	expires := time.Now().UTC().Add(time.Duration(cfg.Image.TemporaryURLTTLSeconds) * time.Second).Unix()
	if parsed, err := time.Parse(time.RFC3339, item.ExpiresAt); err == nil {
		expires = parsed.Unix()
	}
	signature := s.sign(cfg.SigningKey, item.ID, expires)
	return fmt.Sprintf("%s/generated/%s?expires=%d&sig=%s",
		base, url.PathEscape(item.ID+ExtensionForMIME(item.MIME)), expires, signature)
}

func (s *Storage) Verify(id, expiresRaw, signature string) bool {
	expires, err := strconv.ParseInt(expiresRaw, 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return false
	}
	cfg := s.config.Snapshot()
	expected := s.sign(cfg.SigningKey, id, expires)
	return hmac.Equal([]byte(signature), []byte(expected))
}

func (s *Storage) Cleanup(ctx context.Context) (int, error) {
	cutoff := time.Now().UTC().Format(time.RFC3339)
	items, err := s.db.ListExpiredTemporaryFiles(ctx, cutoff, 500)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, item := range items {
		if err := s.Remove(item.RelPath); err != nil {
			continue
		}
		if err := s.db.DeleteTemporaryFile(ctx, item.ID); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *Storage) CleanupLogs(ctx context.Context) (int64, error) {
	days := s.config.Snapshot().Image.LogRetentionDays
	if days < 1 {
		days = 30
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	return s.db.PurgeRequestLogsBefore(ctx, cutoff)
}

func (s *Storage) RunMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.Cleanup(ctx)
			_, _ = s.CleanupLogs(ctx)
		}
	}
}

func (s *Storage) resolve(rel string) (string, error) {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || strings.Contains(rel, "..") {
		return "", fmt.Errorf("invalid temporary image path")
	}
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	root, _ := filepath.Abs(s.root)
	target, _ := filepath.Abs(full)
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid temporary image path")
	}
	return target, nil
}

func (s *Storage) sign(key, id string, expires int64) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(id))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write([]byte(strconv.FormatInt(expires, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomID() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func ExtensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
