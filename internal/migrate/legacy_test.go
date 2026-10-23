package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

func TestLegacyAccountImport(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`[
  {"access_token":"token-1","email":"one@example.com","type":"Plus","quota":7,"status":"正常"},
  {"access_token":"token-2","email":"two@example.com","type":"Team","quota":0,"status":"限流"}
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count, err := LegacyAccountFiles(context.Background(), db, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 accounts, got %d", count)
	}
	items, err := db.ListAccounts(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("list accounts: %v count=%d", err, len(items))
	}
	if items[0].Status != store.AccountNormal && items[0].Status != store.AccountLimited {
		t.Fatalf("unexpected status: %s", items[0].Status)
	}
}
