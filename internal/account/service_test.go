package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

func TestRefreshTokenKeepaliveDue(t *testing.T) {
	if !refreshTokenKeepaliveDue(store.Account{
		RefreshToken:       "refresh",
		LastTokenRefreshAt: time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339),
	}) {
		t.Fatal("eight-day-old refresh token should be due")
	}
	if refreshTokenKeepaliveDue(store.Account{
		RefreshToken:       "refresh",
		LastTokenRefreshAt: time.Now().UTC().Add(-6 * 24 * time.Hour).Format(time.RFC3339),
	}) {
		t.Fatal("six-day-old refresh token should not be due")
	}
}

func TestCollectImportItemsFromCPAWrapper(t *testing.T) {
	value := map[string]any{
		"items": []any{
			map[string]any{"accessToken": "a", "refresh_token": "r"},
			map[string]any{"access_token": "b", "type": "Plus"},
		},
	}
	items := collectImportItems(value)
	if len(items) != 2 || items[0].AccessToken != "a" || items[1].AccessToken != "b" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestParseImportJSONNestedWrapper(t *testing.T) {
	data := []byte(`{
		"data": {
			"items": [
				{"access_token": "token-a", "email": "a@example.com"},
				{"accessToken": "token-b", "type": "Plus"}
			]
		}
	}`)
	items, err := ParseImportJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].AccessToken != "token-a" || items[1].AccessToken != "token-b" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestParseImportJSONTokenList(t *testing.T) {
	items, err := ParseImportJSON([]byte(`{"tokens":["token-a","token-b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].AccessToken != "token-a" || items[1].AccessToken != "token-b" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestRefreshOnePreservesDisabledStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "disabled@example.com", "id": "u1"})
		case r.URL.Path == "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"limits_progress": []any{map[string]any{
					"feature_name": "image_gen", "remaining": 5, "reset_after": "2026-10-26T00:00:00Z",
				}},
			})
		case r.URL.Path == "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accounts": map[string]any{
					"default": map[string]any{
						"account": map[string]any{"plan_type": "plus"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ProviderBaseURL = upstream.URL
	cfgStore := config.NewStore(filepath.Join(dir, "config.yaml"), &cfg)
	db, err := store.Open(cfg.DBPath(), cfg.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account, err := db.AddAccount(context.Background(), store.Account{
		AccessToken: "access-token", Status: store.AccountDisabled, Quota: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, cfgStore)
	if err := service.RefreshOne(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != store.AccountDisabled {
		t.Fatalf("disabled account was changed to %q", updated.Status)
	}
	if updated.Quota != 5 || updated.Email != "disabled@example.com" {
		t.Fatalf("account info was not refreshed: %#v", updated)
	}
}
