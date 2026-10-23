package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	AccountNormal   = "normal"
	AccountLimited  = "limited"
	AccountError    = "error"
	AccountDisabled = "disabled"

	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCanceled  = "canceled"
)

type Account struct {
	ID               string `json:"id"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	IDToken          string `json:"id_token,omitempty"`
	Email            string `json:"email,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	Type             string `json:"type"`
	SourceType       string `json:"source_type"`
	Status           string `json:"status"`
	Quota            int    `json:"quota"`
	RestoreAt        string `json:"restore_at,omitempty"`
	Proxy            string `json:"proxy,omitempty"`
	SuccessCount     int    `json:"success"`
	FailCount        int    `json:"fail"`
	LastUsedAt       string `json:"last_used_at,omitempty"`
	LastRefreshError string `json:"last_refresh_error,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

func (a Account) Public() Account {
	out := a
	out.AccessToken = maskToken(a.AccessToken)
	out.RefreshToken = ""
	out.IDToken = ""
	return out
}

type Job struct {
	ID             string          `json:"id"`
	ClientTaskID   string          `json:"client_task_id,omitempty"`
	Owner          string          `json:"owner"`
	Mode           string          `json:"mode"`
	Prompt         string          `json:"prompt"`
	Model          string          `json:"model"`
	Size           string          `json:"size,omitempty"`
	Quality        string          `json:"quality"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	NextRunAt      string          `json:"next_run_at,omitempty"`
	LeaseUntil     string          `json:"lease_until,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Progress       string          `json:"progress,omitempty"`
	Error          string          `json:"error,omitempty"`
	ResultJSON     json.RawMessage `json:"-"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	StartedAt      string          `json:"started_at,omitempty"`
	FinishedAt     string          `json:"finished_at,omitempty"`
	Images         []Image         `json:"images,omitempty"`
}

type Image struct {
	ID        string `json:"id"`
	JobID     string `json:"job_id"`
	Owner     string `json:"owner"`
	RelPath   string `json:"rel_path"`
	MIME      string `json:"mime"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Bytes     int64  `json:"bytes"`
	CreatedAt string `json:"created_at"`
}

type JobInput struct {
	JobID   string `json:"job_id"`
	Ordinal int    `json:"ordinal"`
	RelPath string `json:"rel_path"`
	MIME    string `json:"mime"`
}

type Stats struct {
	AccountsTotal    int   `json:"accounts_total"`
	AccountsActive   int   `json:"accounts_active"`
	AccountsLimited  int   `json:"accounts_limited"`
	AccountsError    int   `json:"accounts_error"`
	AccountsDisabled int   `json:"accounts_disabled"`
	QuotaTotal       int   `json:"quota_total"`
	JobsQueued       int   `json:"jobs_queued"`
	JobsRunning      int   `json:"jobs_running"`
	JobsSucceeded    int64 `json:"jobs_succeeded"`
	JobsFailed       int64 `json:"jobs_failed"`
	ImagesTotal      int64 `json:"images_total"`
	ImageBytes       int64 `json:"image_bytes"`
}

type Store struct {
	db     *sql.DB
	secret []byte
}

func Open(path string, secretSeed ...string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA cache_size=-2000",
		"PRAGMA temp_store=FILE",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	var secret []byte
	if len(secretSeed) > 0 && strings.TrimSpace(secretSeed[0]) != "" {
		secret = deriveSecretKey(strings.TrimSpace(secretSeed[0]))
	}
	s := &Store{db: db, secret: secret}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migratePlaintextSecrets(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) RecoverRunningJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE image_jobs SET status='queued', lease_until='', progress='', updated_at=? WHERE status='running' AND (lease_until = '' OR lease_until >= ?)`, now(), now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS accounts (
	id TEXT PRIMARY KEY,
	access_token TEXT NOT NULL UNIQUE,
	access_token_enc TEXT NOT NULL DEFAULT '',
	refresh_token TEXT NOT NULL DEFAULT '',
	refresh_token_enc TEXT NOT NULL DEFAULT '',
	id_token TEXT NOT NULL DEFAULT '',
	id_token_enc TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	user_id TEXT NOT NULL DEFAULT '',
	account_type TEXT NOT NULL DEFAULT 'free',
	source_type TEXT NOT NULL DEFAULT 'web',
	status TEXT NOT NULL DEFAULT 'normal',
	quota INTEGER NOT NULL DEFAULT 0,
	restore_at TEXT NOT NULL DEFAULT '',
	proxy TEXT NOT NULL DEFAULT '',
	success_count INTEGER NOT NULL DEFAULT 0,
	fail_count INTEGER NOT NULL DEFAULT 0,
	last_used_at TEXT NOT NULL DEFAULT '',
	last_refresh_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id TEXT NOT NULL,
	event TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS image_jobs (
	id TEXT PRIMARY KEY,
	client_task_id TEXT NOT NULL DEFAULT '',
	owner TEXT NOT NULL DEFAULT 'admin',
	mode TEXT NOT NULL DEFAULT 'generate',
	prompt TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT 'gpt-image-2',
	size TEXT NOT NULL DEFAULT '',
	quality TEXT NOT NULL DEFAULT 'auto',
	status TEXT NOT NULL DEFAULT 'queued',
	attempts INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 3,
	next_run_at TEXT NOT NULL DEFAULT '',
	lease_until TEXT NOT NULL DEFAULT '',
	conversation_id TEXT NOT NULL DEFAULT '',
	progress TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	result_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	started_at TEXT NOT NULL DEFAULT '',
	finished_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_image_jobs_queue ON image_jobs(status, next_run_at, created_at);
CREATE INDEX IF NOT EXISTS idx_image_jobs_owner ON image_jobs(owner, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_image_jobs_client ON image_jobs(owner, client_task_id) WHERE client_task_id <> '';

CREATE TABLE IF NOT EXISTS images (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL,
	owner TEXT NOT NULL,
	rel_path TEXT NOT NULL UNIQUE,
	mime TEXT NOT NULL DEFAULT 'image/png',
	width INTEGER NOT NULL DEFAULT 0,
	height INTEGER NOT NULL DEFAULT 0,
	bytes INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_inputs (
	job_id TEXT NOT NULL,
	ordinal INTEGER NOT NULL,
	rel_path TEXT NOT NULL,
	mime TEXT NOT NULL DEFAULT 'image/png',
	PRIMARY KEY (job_id, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_images_created ON images(created_at);
CREATE INDEX IF NOT EXISTS idx_images_owner ON images(owner, created_at);

CREATE TABLE IF NOT EXISTS api_key_usage (
	key_name TEXT PRIMARY KEY,
	requests INTEGER NOT NULL DEFAULT 0,
	failures INTEGER NOT NULL DEFAULT 0,
	last_used_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS request_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{"access_token_enc", "ALTER TABLE accounts ADD COLUMN access_token_enc TEXT NOT NULL DEFAULT ''"},
		{"refresh_token_enc", "ALTER TABLE accounts ADD COLUMN refresh_token_enc TEXT NOT NULL DEFAULT ''"},
		{"id_token_enc", "ALTER TABLE accounts ADD COLUMN id_token_enc TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, "accounts", column.name, column.ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, ddl string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err == nil && name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, ddl)
	return err
}

func (s *Store) migratePlaintextSecrets(ctx context.Context) error {
	if len(s.secret) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, access_token, refresh_token, id_token,
		access_token_enc, refresh_token_enc, id_token_enc FROM accounts`)
	if err != nil {
		return err
	}
	type item struct {
		id, access, refresh, idToken, accessEnc, refreshEnc, idEnc string
	}
	var items []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.access, &v.refresh, &v.idToken, &v.accessEnc, &v.refreshEnc, &v.idEnc); err != nil {
			rows.Close()
			return err
		}
		if v.accessEnc == "" && v.access != "" && !strings.HasPrefix(v.access, "sha256:") {
			items = append(items, v)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, v := range items {
		accessEnc, err := seal(s.secret, v.access)
		if err != nil {
			return err
		}
		refreshEnc, err := seal(s.secret, v.refresh)
		if err != nil {
			return err
		}
		idEnc, err := seal(s.secret, v.idToken)
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE accounts SET access_token=?, access_token_enc=?, refresh_token=?, refresh_token_enc=?, id_token=?, id_token_enc=? WHERE id=?`,
			tokenLookup(s.secret, v.access), accessEnc, "", refreshEnc, "", idEnc, v.id)
		if err != nil {
			return err
		}
	}
	return nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func (s *Store) AddAccount(ctx context.Context, a Account) (Account, error) {
	nowValue := now()
	if a.ID == "" {
		a.ID = newID()
	}
	if a.Type == "" {
		a.Type = "free"
	}
	if a.SourceType == "" {
		a.SourceType = "web"
	}
	if a.Status == "" {
		a.Status = AccountNormal
	}
	if a.CreatedAt == "" {
		a.CreatedAt = nowValue
	}
	a.UpdatedAt = nowValue
	accessEnc, err := s.encrypt(a.AccessToken)
	if err != nil {
		return Account{}, err
	}
	refreshEnc, err := s.encrypt(a.RefreshToken)
	if err != nil {
		return Account{}, err
	}
	idEnc, err := s.encrypt(a.IDToken)
	if err != nil {
		return Account{}, err
	}
	lookup := tokenLookup(s.secret, a.AccessToken)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO accounts (id, access_token, access_token_enc, refresh_token, refresh_token_enc, id_token, id_token_enc, email, user_id, account_type,
	source_type, status, quota, restore_at, proxy, success_count, fail_count, last_used_at,
	last_refresh_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(access_token) DO NOTHING`,
		a.ID, lookup, accessEnc, "", refreshEnc, "", idEnc, a.Email, a.UserID, a.Type, a.SourceType,
		a.Status, a.Quota, a.RestoreAt, a.Proxy, a.SuccessCount, a.FailCount, a.LastUsedAt,
		a.LastRefreshError, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return Account{}, err
	}
	return s.GetAccountByToken(ctx, a.AccessToken)
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, accountSelect+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccountByToken(ctx context.Context, token string) (Account, error) {
	return s.scanAccount(s.db.QueryRowContext(ctx, accountSelect+` WHERE access_token = ?`, tokenLookup(s.secret, token)))
}

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	return s.scanAccount(s.db.QueryRowContext(ctx, accountSelect+` WHERE id = ?`, id))
}

func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	a.UpdatedAt = now()
	accessEnc, err := s.encrypt(a.AccessToken)
	if err != nil {
		return err
	}
	refreshEnc, err := s.encrypt(a.RefreshToken)
	if err != nil {
		return err
	}
	idEnc, err := s.encrypt(a.IDToken)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE accounts SET access_token=?, refresh_token=?, id_token=?, email=?,
		access_token_enc=?, refresh_token_enc=?, id_token_enc=?,
		user_id=?, account_type=?, source_type=?, status=?, quota=?, restore_at=?, proxy=?,
		success_count=?, fail_count=?, last_used_at=?, last_refresh_error=?, updated_at=? WHERE id=?`,
		tokenLookup(s.secret, a.AccessToken), "", "", a.Email, accessEnc, refreshEnc, idEnc, a.UserID, a.Type, a.SourceType,
		a.Status, a.Quota, a.RestoreAt, a.Proxy, a.SuccessCount, a.FailCount, a.LastUsedAt,
		a.LastRefreshError, a.UpdatedAt, a.ID)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	return err
}

func (s *Store) AddAccountEvent(ctx context.Context, accountID, event, message string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO account_events(account_id, event, message, created_at) VALUES(?,?,?,?)`,
		accountID, event, message, now())
}

func (s *Store) CreateJob(ctx context.Context, j Job) (Job, bool, error) {
	if j.ID == "" {
		j.ID = newID()
	}
	if j.Owner == "" {
		j.Owner = "admin"
	}
	if j.Mode == "" {
		j.Mode = "generate"
	}
	if j.Quality == "" {
		j.Quality = "auto"
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = 3
	}
	j.Status = JobQueued
	j.CreatedAt = now()
	j.UpdatedAt = j.CreatedAt
	j.NextRunAt = j.CreatedAt
	_, err := s.db.ExecContext(ctx, `INSERT INTO image_jobs
		(id, client_task_id, owner, mode, prompt, model, size, quality, status, attempts, max_attempts,
		 next_run_at, lease_until, conversation_id, progress, error, result_json, created_at, updated_at, started_at, finished_at)
		VALUES(?,?,?,?,?,?,?,?,'queued',0,?,?, '', '', '', '', '', ?,?,'','')`,
		j.ID, j.ClientTaskID, j.Owner, j.Mode, j.Prompt, j.Model, j.Size, j.Quality, j.MaxAttempts, j.NextRunAt, j.CreatedAt, j.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") && j.ClientTaskID != "" {
			existing, getErr := s.GetJobByClientID(ctx, j.Owner, j.ClientTaskID)
			return existing, false, getErr
		}
		return Job{}, false, err
	}
	return j, true, nil
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE id = ?`, id))
}

func (s *Store) GetJobByClientID(ctx context.Context, owner, clientID string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE owner = ? AND client_task_id = ? ORDER BY created_at DESC LIMIT 1`, owner, clientID))
}

const jobSelect = `SELECT id, client_task_id, owner, mode, prompt, model, size, quality, status,
	attempts, max_attempts, next_run_at, lease_until, conversation_id, progress, error, result_json,
	created_at, updated_at, started_at, finished_at FROM image_jobs`

const accountSelect = `SELECT id, access_token, access_token_enc, refresh_token, refresh_token_enc,
	id_token, id_token_enc, email, user_id, account_type, source_type, status, quota, restore_at,
	proxy, success_count, fail_count, last_used_at, last_refresh_error, created_at, updated_at FROM accounts`

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) encrypt(value string) (string, error) {
	if len(s.secret) == 0 || value == "" {
		return "", nil
	}
	return seal(s.secret, value)
}

func (s *Store) scanAccount(row rowScanner) (Account, error) {
	var a Account
	var accessEnc, refreshEnc, idEnc string
	err := row.Scan(&a.ID, &a.AccessToken, &accessEnc, &a.RefreshToken, &refreshEnc,
		&a.IDToken, &idEnc, &a.Email, &a.UserID, &a.Type, &a.SourceType, &a.Status, &a.Quota,
		&a.RestoreAt, &a.Proxy, &a.SuccessCount, &a.FailCount, &a.LastUsedAt,
		&a.LastRefreshError, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	if accessEnc != "" {
		a.AccessToken, err = open(s.secret, accessEnc)
		if err != nil {
			return a, err
		}
	}
	if refreshEnc != "" {
		a.RefreshToken, err = open(s.secret, refreshEnc)
		if err != nil {
			return a, err
		}
	}
	if idEnc != "" {
		a.IDToken, err = open(s.secret, idEnc)
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func scanJob(row rowScanner) (Job, error) {
	var j Job
	var result string
	err := row.Scan(&j.ID, &j.ClientTaskID, &j.Owner, &j.Mode, &j.Prompt, &j.Model, &j.Size, &j.Quality,
		&j.Status, &j.Attempts, &j.MaxAttempts, &j.NextRunAt, &j.LeaseUntil, &j.ConversationID,
		&j.Progress, &j.Error, &result, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return j, err
	}
	if result != "" {
		j.ResultJSON = json.RawMessage(result)
	}
	return j, nil
}

func (s *Store) ListJobs(ctx context.Context, owner string, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE owner = ? ORDER BY created_at DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ListAllJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, jobSelect+` ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ClaimJob(ctx context.Context, lease time.Duration) (Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	nowValue := now()
	// Requeue leases that were abandoned by a crashed process.
	_, _ = tx.ExecContext(ctx, `UPDATE image_jobs SET status='queued', lease_until='', updated_at=? WHERE status='running' AND lease_until <> '' AND lease_until < ?`, nowValue, nowValue)
	row := tx.QueryRowContext(ctx, jobSelect+` WHERE status='queued' AND next_run_at <= ? ORDER BY created_at LIMIT 1`, nowValue)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, tx.Commit()
	}
	if err != nil {
		return Job{}, false, err
	}
	leaseValue := time.Now().UTC().Add(lease).Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `UPDATE image_jobs SET status='running', attempts=attempts+1, started_at=?, lease_until=?, updated_at=? WHERE id=? AND status='queued'`, nowValue, leaseValue, nowValue, j.ID)
	if err != nil {
		return Job{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Job{}, false, tx.Commit()
	}
	j.Status = JobRunning
	j.Attempts++
	j.StartedAt = nowValue
	j.LeaseUntil = leaseValue
	j.UpdatedAt = nowValue
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

func (s *Store) UpdateJobProgress(ctx context.Context, id, progress string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE image_jobs SET progress=?, updated_at=?, lease_until=? WHERE id=?`, progress, now(), time.Now().UTC().Add(30*time.Minute).Format(time.RFC3339), id)
	return err
}

func (s *Store) FinishJob(ctx context.Context, id string, result any, errText string) error {
	status := JobSucceeded
	finished := now()
	if errText != "" {
		status = JobFailed
	}
	resultJSON := ""
	if result != nil {
		if b, err := json.Marshal(result); err == nil {
			resultJSON = string(b)
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE image_jobs SET status=?, result_json=?, error=?, progress='', lease_until='', finished_at=?, updated_at=? WHERE id=?`,
		status, resultJSON, errText, finished, finished, id)
	return err
}

func (s *Store) RetryJob(ctx context.Context, id string, delay time.Duration, errText string) error {
	next := time.Now().UTC().Add(delay).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE image_jobs SET status='queued', error=?, progress='', next_run_at=?, lease_until='', updated_at=? WHERE id=?`, errText, next, now(), id)
	return err
}

func (s *Store) CancelJob(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE image_jobs SET status='canceled', updated_at=? WHERE id=? AND status IN ('queued','running')`, now(), id)
	return err
}

func (s *Store) AddImage(ctx context.Context, img Image) error {
	if img.ID == "" {
		img.ID = newID()
	}
	if img.CreatedAt == "" {
		img.CreatedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO images(id, job_id, owner, rel_path, mime, width, height, bytes, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		img.ID, img.JobID, img.Owner, img.RelPath, img.MIME, img.Width, img.Height, img.Bytes, img.CreatedAt)
	return err
}

func (s *Store) AddJobInput(ctx context.Context, input JobInput) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_inputs(job_id, ordinal, rel_path, mime) VALUES(?,?,?,?)
		ON CONFLICT(job_id, ordinal) DO UPDATE SET rel_path=excluded.rel_path, mime=excluded.mime`,
		input.JobID, input.Ordinal, input.RelPath, input.MIME)
	return err
}

func (s *Store) ListJobInputs(ctx context.Context, jobID string) ([]JobInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, ordinal, rel_path, mime FROM job_inputs WHERE job_id=? ORDER BY ordinal`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobInput
	for rows.Next() {
		var input JobInput
		if err := rows.Scan(&input.JobID, &input.Ordinal, &input.RelPath, &input.MIME); err != nil {
			return nil, err
		}
		out = append(out, input)
	}
	return out, rows.Err()
}

func (s *Store) ListImages(ctx context.Context, owner string, limit int) ([]Image, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, owner, rel_path, mime, width, height, bytes, created_at FROM images WHERE owner=? ORDER BY created_at DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var img Image
		if err := rows.Scan(&img.ID, &img.JobID, &img.Owner, &img.RelPath, &img.MIME, &img.Width, &img.Height, &img.Bytes, &img.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

func (s *Store) ListAllImages(ctx context.Context, limit int) ([]Image, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, owner, rel_path, mime, width, height, bytes, created_at FROM images ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var img Image
		if err := rows.Scan(&img.ID, &img.JobID, &img.Owner, &img.RelPath, &img.MIME, &img.Width, &img.Height, &img.Bytes, &img.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

func (s *Store) ListImagesByJob(ctx context.Context, jobID string) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, owner, rel_path, mime, width, height, bytes, created_at FROM images WHERE job_id=? ORDER BY created_at`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var img Image
		if err := rows.Scan(&img.ID, &img.JobID, &img.Owner, &img.RelPath, &img.MIME, &img.Width, &img.Height, &img.Bytes, &img.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

func (s *Store) GetImageByPath(ctx context.Context, rel string) (Image, error) {
	var img Image
	err := s.db.QueryRowContext(ctx, `SELECT id, job_id, owner, rel_path, mime, width, height, bytes, created_at FROM images WHERE rel_path=?`, rel).
		Scan(&img.ID, &img.JobID, &img.Owner, &img.RelPath, &img.MIME, &img.Width, &img.Height, &img.Bytes, &img.CreatedAt)
	return img, err
}

func (s *Store) ListImagesBefore(ctx context.Context, cutoff string, limit int) ([]Image, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, owner, rel_path, mime, width, height, bytes, created_at FROM images WHERE created_at < ? ORDER BY created_at LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var img Image
		if err := rows.Scan(&img.ID, &img.JobID, &img.Owner, &img.RelPath, &img.MIME, &img.Width, &img.Height, &img.Bytes, &img.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

func (s *Store) DeleteImageRecord(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id = ?`, id)
	return err
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='normal' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='limited' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='disabled' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='normal' THEN quota ELSE 0 END),0) FROM accounts`).
		Scan(&st.AccountsTotal, &st.AccountsActive, &st.AccountsLimited, &st.AccountsError, &st.AccountsDisabled, &st.QuotaTotal)
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status='queued' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='running' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='succeeded' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),0) FROM image_jobs`).
		Scan(&st.JobsQueued, &st.JobsRunning, &st.JobsSucceeded, &st.JobsFailed)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM images`).Scan(&st.ImagesTotal, &st.ImageBytes)
	return st, nil
}

func (s *Store) RecordAPIKeyUse(ctx context.Context, name string, failed bool) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO api_key_usage(key_name, requests, failures, last_used_at) VALUES(?,1,?,?)
		ON CONFLICT(key_name) DO UPDATE SET requests=requests+1, failures=failures+excluded.failures, last_used_at=excluded.last_used_at`,
		name, boolInt(failed), now())
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func newID() string {
	// UUID-like, dependency-free id. The timestamp prefix makes list output
	// easier to inspect during local operation.
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), randomHex(12))
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}

func maskToken(v string) string {
	if len(v) <= 12 {
		return "***"
	}
	return v[:6] + "..." + v[len(v)-4:]
}
