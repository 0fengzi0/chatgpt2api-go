package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

// LegacyAccountFiles imports the account snapshot used by the Python version.
// It is intentionally only run on an empty database.
func LegacyAccountFiles(ctx context.Context, db *store.Store, dataDir string) (int, error) {
	current, err := db.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}
	if len(current) > 0 {
		return 0, nil
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "accounts.json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return 0, err
	}
	imported := 0
	for _, raw := range items {
		access := firstString(raw, "access_token", "accessToken")
		if access == "" {
			continue
		}
		a := store.Account{
			AccessToken:  access,
			RefreshToken: firstString(raw, "refresh_token", "refreshToken"),
			IDToken:      firstString(raw, "id_token", "idToken"),
			Email:        firstString(raw, "email"),
			UserID:       firstString(raw, "user_id", "userId"),
			Type:         firstString(raw, "type", "plan_type"),
			SourceType:   firstString(raw, "source_type", "export_type"),
			Status:       mapStatus(firstString(raw, "status")),
			Quota:        firstInt(raw, "quota"),
			RestoreAt:    firstString(raw, "restore_at"),
			Proxy:        firstString(raw, "proxy"),
			SuccessCount: firstInt(raw, "success"),
			FailCount:    firstInt(raw, "fail"),
			LastUsedAt:   firstString(raw, "last_used_at"),
			CreatedAt:    firstString(raw, "created_at"),
		}
		if a.Type == "" {
			a.Type = "free"
		}
		if a.SourceType == "" {
			a.SourceType = "web"
		}
		if _, err := db.AddAccount(ctx, a); err != nil {
			return imported, err
		}
		imported++
	}
	return imported, nil
}

// LegacyAuthKeys merges user keys from the old auth_keys.json into the new
// config-managed API key list. Admin keys are intentionally skipped.
func LegacyAuthKeys(path string, cfgStore *config.Store) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, err
	}
	imported := 0
	err = cfgStore.Update(func(cfg *config.Config) {
		existing := map[string]bool{}
		for _, item := range cfg.APIKeys {
			existing[strings.ToLower(item.KeyHash)] = true
		}
		for _, item := range raw.Items {
			if strings.ToLower(firstString(item, "role")) != "user" {
				continue
			}
			hash := strings.TrimSpace(firstString(item, "key_hash"))
			if hash == "" || existing["sha256:"+strings.ToLower(hash)] {
				continue
			}
			name := firstString(item, "name")
			if name == "" {
				name = "legacy-user"
			}
			cfg.APIKeys = append(cfg.APIKeys, config.APIKey{
				Name:      name,
				KeyHash:   "sha256:" + strings.ToLower(hash),
				Enabled:   true,
				CreatedAt: firstString(item, "created_at"),
			})
			existing["sha256:"+strings.ToLower(hash)] = true
			imported++
		}
	})
	return imported, err
}

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstInt(item map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := item[key].(type) {
		case float64:
			return int(value)
		case int:
			return value
		case json.Number:
			n, _ := value.Int64()
			return int(n)
		}
	}
	return 0
}

func mapStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "disabled", "禁用":
		return store.AccountDisabled
	case "error", "异常":
		return store.AccountError
	case "limited", "限流":
		return store.AccountLimited
	default:
		return store.AccountNormal
	}
}
