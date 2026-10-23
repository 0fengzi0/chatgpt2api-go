package image

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
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
	root      string
	inputRoot string
	secret    []byte
	baseURL   string
	maxPixels int64
}

func New(cfg config.Config) (*Storage, error) {
	root := cfg.ImageDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	secret := sha256.Sum256([]byte(cfg.SigningKey))
	return &Storage{
		root:      root,
		inputRoot: filepath.Join(cfg.DataDirPath(), "uploads"),
		secret:    secret[:],
		baseURL:   strings.TrimRight(cfg.PublicBaseURL, "/"),
		maxPixels: cfg.Image.MaxImagePixels,
	}, nil
}

func (s *Storage) SaveInput(_ context.Context, jobID string, ordinal int, data []byte, mime string) (string, error) {
	dir := filepath.Join(s.inputRoot, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%03d%s", ordinal, extensionForMime(mime))
	rel := filepath.ToSlash(filepath.Join(jobID, name))
	full := filepath.Join(s.inputRoot, filepath.FromSlash(rel))
	if err := os.WriteFile(full, data, 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

func (s *Storage) OpenInput(rel string) (io.ReadCloser, string, error) {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	full := filepath.Join(s.inputRoot, filepath.FromSlash(rel))
	root, _ := filepath.Abs(s.inputRoot)
	target, _ := filepath.Abs(full)
	if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return nil, "", fmt.Errorf("invalid upload path")
	}
	f, err := os.Open(target)
	if err != nil {
		return nil, "", err
	}
	return f, mimeForExt(filepath.Ext(target)), nil
}

func (s *Storage) Save(_ context.Context, owner, jobID string, data []byte, mime string) (store.Image, error) {
	if int64(len(data)) == 0 {
		return store.Image{}, fmt.Errorf("empty image")
	}
	if s.maxPixels > 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			if int64(cfg.Width)*int64(cfg.Height) > s.maxPixels {
				return store.Image{}, fmt.Errorf("image too large: %dx%d", cfg.Width, cfg.Height)
			}
		}
	}
	now := time.Now()
	relDir := filepath.Join(now.Format("2006"), now.Format("01"), now.Format("02"))
	name := jobID + "-" + randomToken(8) + extensionForMime(mime)
	rel := filepath.ToSlash(filepath.Join(relDir, name))
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return store.Image{}, err
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return store.Image{}, err
	}
	width, height := 0, 0
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		width, height = cfg.Width, cfg.Height
	}
	return store.Image{
		JobID:     jobID,
		Owner:     owner,
		RelPath:   rel,
		MIME:      mimeOrDefault(mime),
		Width:     width,
		Height:    height,
		Bytes:     int64(len(data)),
		CreatedAt: now.UTC().Format(time.RFC3339),
	}, nil
}

func (s *Storage) Open(rel string) (io.ReadCloser, string, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, "", err
	}
	return f, mimeForExt(filepath.Ext(full)), nil
}

func (s *Storage) Remove(rel string) error {
	full, err := s.resolve(rel)
	if err != nil {
		return err
	}
	err = os.Remove(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Storage) SignedURL(origin, rel string, ttl time.Duration) string {
	base := s.baseURL
	if base == "" {
		base = strings.TrimRight(origin, "/")
	}
	expires := time.Now().Add(ttl).Unix()
	sig := s.sign(rel, expires)
	return fmt.Sprintf("%s/images/%s?expires=%d&sig=%s", base, url.PathEscape(rel), expires, sig)
}

func (s *Storage) Verify(rel, expiresRaw, sig string) bool {
	expires, err := strconv.ParseInt(expiresRaw, 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.sign(rel, expires)))
}

func (s *Storage) sign(rel string, expires int64) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(rel))
	mac.Write([]byte(":"))
	mac.Write([]byte(strconv.FormatInt(expires, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Storage) resolve(rel string) (string, error) {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	root, _ := filepath.Abs(s.root)
	target, _ := filepath.Abs(full)
	if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid image path")
	}
	return target, nil
}

func mimeOrDefault(mime string) string {
	mime = strings.TrimSpace(strings.Split(mime, ";")[0])
	if strings.HasPrefix(mime, "image/") {
		return mime
	}
	return "image/png"
}

func extensionForMime(mime string) string {
	switch mimeOrDefault(mime) {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func randomToken(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		for i := range buf {
			buf[i] = alphabet[(int(time.Now().UnixNano())+i)%len(alphabet)]
		}
		return string(buf)
	}
	for i := range buf {
		buf[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(buf)
}
