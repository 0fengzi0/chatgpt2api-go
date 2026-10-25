package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
)

func TestManagementAcceptsRemoteRequestWithValidKey(t *testing.T) {
	hash, err := config.HashSecret("management-secret")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.RemoteManagement.SecretKey = hash
	manager := New(config.NewStore("config.yaml", &cfg))

	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.RemoteAddr = "203.0.113.10:12345"
	request.Header.Set("X-Management-Key", "management-secret")
	ok, enabled := manager.Management(request)
	if !enabled || !ok {
		t.Fatalf("valid remote key rejected: enabled=%v ok=%v", enabled, ok)
	}
	request.Header.Set("X-Management-Key", "wrong")
	ok, enabled = manager.Management(request)
	if !enabled || ok {
		t.Fatalf("wrong remote key accepted: enabled=%v ok=%v", enabled, ok)
	}
}

func TestAPIKeyLifecycleAndDuplicateName(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.APIKeys = nil
	store := config.NewStore(filepath.Join(dir, "config.yaml"), &cfg)
	manager := New(store)
	raw, err := manager.CreateAPIKey("client")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+raw)
	name, err := manager.AuthenticateAPIKey(request)
	if err != nil || name != "client" {
		t.Fatalf("authenticate key: name=%q err=%v", name, err)
	}
	if _, err := manager.CreateAPIKey("client"); err == nil {
		t.Fatal("duplicate key name was accepted")
	}
	enabled := false
	if err := manager.UpdateAPIKey("client", &enabled, ""); err != nil {
		t.Fatal(err)
	}
	_, err = manager.AuthenticateAPIKey(request)
	if !errors.Is(err, ErrDisabledAPIKey) {
		t.Fatalf("disabled key error = %v", err)
	}
}
