package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
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
	cfg := m.store.Snapshot()
	candidate := bearer(r.Header.Get("Authorization"))
	if candidate == "" {
		candidate = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if candidate == "" {
		return "", false
	}
	for _, item := range cfg.APIKeys {
		if config.VerifyAPIKey(item, candidate) {
			return item.Name, true
		}
	}
	return "", false
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
	return m.store.Update(func(cfg *config.Config) {
		for i := range cfg.APIKeys {
			if cfg.APIKeys[i].Name != name {
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
	return m.store.Update(func(cfg *config.Config) {
		for i := range cfg.APIKeys {
			if cfg.APIKeys[i].Name == name {
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
