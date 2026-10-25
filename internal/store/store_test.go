package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestJobLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	job, created, err := s.CreateJob(ctx, Job{Owner: "admin", Prompt: "test", Model: "gpt-image-2", Quality: "auto"})
	if err != nil || !created {
		t.Fatalf("create job: %v created=%v", err, created)
	}
	claimed, ok, err := s.ClaimJob(ctx, time.Minute)
	if err != nil || !ok || claimed.ID != job.ID || claimed.Status != JobRunning {
		t.Fatalf("claim job: %#v ok=%v err=%v", claimed, ok, err)
	}
	if err := s.FinishJob(ctx, job.ID, map[string]any{"ok": true}, ""); err != nil {
		t.Fatal(err)
	}
	done, err := s.GetJob(ctx, job.ID)
	if err != nil || done.Status != JobSucceeded {
		t.Fatalf("finish job: %#v err=%v", done, err)
	}
}

func TestAccountSecretsAreEncryptedAtRest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "encrypted.db"), "test-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	a, err := s.AddAccount(ctx, Account{AccessToken: "secret-access", RefreshToken: "secret-refresh", IDToken: "secret-id", Type: "Plus"})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT access_token FROM accounts WHERE id=?`, a.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "secret-access" || stored == "" {
		t.Fatalf("access token was not protected: %q", stored)
	}
	got, err := s.GetAccountByToken(ctx, "secret-access")
	if err != nil || got.RefreshToken != "secret-refresh" || got.IDToken != "secret-id" {
		t.Fatalf("decrypt account: %#v err=%v", got, err)
	}
}

func TestRequestLogFilters(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, item := range []RequestLog{
		{RequestID: "a", Owner: "key-a", Model: "gpt-image-2", Mode: "generation", Status: 200},
		{RequestID: "b", Owner: "key-b", Model: "gpt-image-2.5", Mode: "edit", Status: 502},
	} {
		if _, err := s.AddRequestLog(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ListRequestLogsFiltered(ctx, RequestLogFilter{
		Owner: "key-a", Status: "success", Mode: "generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RequestID != "a" {
		t.Fatalf("unexpected filtered logs: %#v", items)
	}
}
