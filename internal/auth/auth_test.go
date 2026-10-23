package auth

import (
	"net/http"
	"net/http/httptest"
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
