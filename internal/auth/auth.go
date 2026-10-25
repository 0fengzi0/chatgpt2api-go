package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
)

var (
	ErrMissingAPIKey  = errors.New("missing API key")
	ErrInvalidAPIKey  = errors.New("invalid API key")
	ErrDisabledAPIKey = errors.New("api key is disabled or expired")
)

type Manager struct {
	store *config.Store
}

func New(store *config.Store) *Manager {
	return &Manager{store: store}
}

func (m *Manager) Management(r *http.Request) (bool, bool) {
	cfg := m.store.Snapshot()
	if strings.TrimSpace(cfg.ManagementSecretKey()) == "" {
		return false, false
	}
	candidate := strings.TrimSpace(r.Header.Get("X-Management-Key"))
	if candidate == "" {
		candidate = bearer(r.Header.Get("Authorization"))
	}
	if candidate == "" {
		return false, true
	}
	return config.VerifySecret(cfg.ManagementSecretKey(), candidate), true
}

func (m *Manager) APIKey(r *http.Request) (string, bool) {
	name, err := m.AuthenticateAPIKey(r)
	return name, err == nil
}

func (m *Manager) AuthenticateAPIKey(r *http.Request) (string, error) {
	cfg := m.store.Snapshot()
	candidate := bearer(r.Header.Get("Authorization"))
	if candidate == "" {
		candidate = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if candidate == "" {
		return "", ErrMissingAPIKey
	}
	for _, item := range cfg.APIKeys {
		if !config.MatchesAPIKey(item, candidate) {
			continue
		}
		if !item.Enabled {
			return item.Name, ErrDisabledAPIKey
		}
		if item.ExpiresAt != "" {
			if expires, err := time.Parse(time.RFC3339, item.ExpiresAt); err == nil && time.Now().After(expires) {
				return item.Name, ErrDisabledAPIKey
			}
		}
		return item.Name, nil
	}
	return "", ErrInvalidAPIKey
}

func (m *Manager) UpdateManagementKey(raw string) error {
	hash, err := config.HashSecret(raw)
	if err != nil {
		return err
	}
	return m.store.Update(func(cfg *config.Config) {
		cfg.RemoteManagement.SecretKey = hash
	})
}

func (m *Manager) CreateAPIKey(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "client"
	}
	cfg := m.store.Snapshot()
	for _, item := range cfg.APIKeys {
		if strings.EqualFold(strings.TrimSpace(item.Name), name) {
			return "", errors.New("api key name already exists")
		}
	}
	raw, err := randomKey("sk-")
	if err != nil {
		return "", err
	}
	err = m.store.Update(func(cfg *config.Config) {
		cfg.APIKeys = append(cfg.APIKeys, config.APIKey{
			Name:    name,
			KeyHash: config.HashAPIKey(raw),
			Enabled: true,
		})
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (m *Manager) UpdateAPIKey(name string, enabled *bool, newName string) error {
	name = strings.TrimSpace(name)
	newName = strings.TrimSpace(newName)
	found := false
	cfg := m.store.Snapshot()
	for _, item := range cfg.APIKeys {
		if strings.EqualFold(strings.TrimSpace(item.Name), name) {
			found = true
			continue
		}
		if newName != "" && strings.EqualFold(strings.TrimSpace(item.Name), newName) {
			return errors.New("api key name already exists")
		}
	}
	if !found {
		return errors.New("api key not found")
	}
	return m.store.Update(func(cfg *config.Config) {
		for i := range cfg.APIKeys {
			if !strings.EqualFold(strings.TrimSpace(cfg.APIKeys[i].Name), name) {
				continue
			}
			if newName != "" {
				cfg.APIKeys[i].Name = newName
			}
			if enabled != nil {
				cfg.APIKeys[i].Enabled = *enabled
			}
		}
	})
}

func (m *Manager) DeleteAPIKey(name string) error {
	found := false
	for _, item := range m.store.Snapshot().APIKeys {
		if strings.EqualFold(strings.TrimSpace(item.Name), name) {
			found = true
			break
		}
	}
	if !found {
		return errors.New("api key not found")
	}
	return m.store.Update(func(cfg *config.Config) {
		for i := range cfg.APIKeys {
			if strings.EqualFold(strings.TrimSpace(cfg.APIKeys[i].Name), name) {
				cfg.APIKeys = append(cfg.APIKeys[:i], cfg.APIKeys[i+1:]...)
				return
			}
		}
	})
}

func randomKey(prefix string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func bearer(header string) string {
	scheme, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}
