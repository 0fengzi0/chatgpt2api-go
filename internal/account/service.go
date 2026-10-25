package account

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

type Service struct {
	store        *store.Store
	config       *config.Store
	baseURL      string
	mu           sync.Mutex
	refreshDueMu sync.Mutex
	active       map[string]int
	clients      sync.Map
}

type ImportItem struct {
	AccessToken    string `json:"access_token"`
	AccessTokenAlt string `json:"accessToken,omitempty"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	IDToken        string `json:"id_token,omitempty"`
	Type           string `json:"type,omitempty"`
	SourceType     string `json:"source_type,omitempty"`
	ExportType     string `json:"export_type,omitempty"`
	Email          string `json:"email,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	Password       string `json:"password,omitempty"`
	Proxy          string `json:"proxy,omitempty"`
	Quota          int    `json:"quota,omitempty"`
	RestoreAt      string `json:"restore_at,omitempty"`
}

type RefreshResult struct {
	Refreshed int             `json:"refreshed"`
	Errors    []string        `json:"errors"`
	Items     []store.Account `json:"items"`
}

type HTTPError struct {
	Status int
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream %s returned HTTP %d: %s", e.Path, e.Status, truncate(e.Body, 300))
}

func NewService(s *store.Store, cfg *config.Store) *Service {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.Snapshot().ProviderBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://chatgpt.com"
	}
	return &Service{store: s, config: cfg, baseURL: baseURL, active: make(map[string]int)}
}

func (s *Service) List(ctx context.Context) ([]store.Account, error) {
	return s.store.ListAccounts(ctx)
}

func (s *Service) Import(ctx context.Context, tokens []string, items []ImportItem) (map[string]any, error) {
	added := 0
	skipped := 0
	refreshed := 0
	var refreshErrors []string
	var addedIDs []string
	seen := make(map[string]struct{})
	for _, item := range items {
		token := firstNonEmpty(item.AccessToken, item.AccessTokenAlt)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			skipped++
			continue
		}
		seen[token] = struct{}{}
		exists, err := s.accountExists(ctx, token)
		if err != nil {
			return nil, err
		}
		if exists {
			skipped++
			continue
		}
		account := store.Account{
			AccessToken:  token,
			RefreshToken: item.RefreshToken,
			IDToken:      item.IDToken,
			Type:         item.Type,
			SourceType:   firstNonEmpty(item.SourceType, item.ExportType),
			Email:        item.Email,
			UserID:       item.UserID,
			Proxy:        item.Proxy,
			Quota:        item.Quota,
			RestoreAt:    item.RestoreAt,
			Status:       store.AccountNormal,
		}
		account = fillFromJWT(account)
		saved, err := s.store.AddAccount(ctx, account)
		if err != nil {
			return nil, err
		}
		s.store.AddAccountEvent(ctx, saved.ID, "imported", "account imported")
		addedIDs = append(addedIDs, saved.ID)
		added++
	}
	for _, raw := range tokens {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			skipped++
			continue
		}
		seen[token] = struct{}{}
		exists, err := s.accountExists(ctx, token)
		if err != nil {
			return nil, err
		}
		if exists {
			skipped++
			continue
		}
		account := fillFromJWT(store.Account{AccessToken: token, Status: store.AccountNormal, SourceType: "web"})
		saved, err := s.store.AddAccount(ctx, account)
		if err != nil {
			return nil, err
		}
		s.store.AddAccountEvent(ctx, saved.ID, "imported", "access token imported")
		addedIDs = append(addedIDs, saved.ID)
		added++
	}
	for _, id := range addedIDs {
		if err := s.RefreshOne(ctx, id); err != nil {
			refreshErrors = append(refreshErrors, fmt.Sprintf("%s: %v", id, err))
		} else {
			refreshed++
		}
	}
	current, _ := s.store.ListAccounts(ctx)
	return map[string]any{
		"added":     added,
		"skipped":   skipped,
		"refreshed": refreshed,
		"errors":    refreshErrors,
		"items":     publicAccounts(current),
	}, nil
}

func (s *Service) accountExists(ctx context.Context, token string) (bool, error) {
	_, err := s.store.GetAccountByToken(ctx, token)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func (s *Service) ImportDirectory(ctx context.Context, dir string) (map[string]any, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("auth directory is empty")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var items []ImportItem
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		for _, item := range collectImportItems(value) {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, errors.New("no account JSON files found in auth directory")
	}
	return s.Import(ctx, nil, items)
}

func ParseImportJSON(data []byte) ([]ImportItem, error) {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse account JSON: %w", err)
	}
	items := collectImportItems(value)
	if len(items) == 0 {
		return nil, errors.New("no access tokens found in account JSON")
	}
	return items, nil
}

func collectImportItems(value any) []ImportItem {
	switch v := value.(type) {
	case []any:
		var out []ImportItem
		for _, item := range v {
			out = append(out, collectImportItems(item)...)
		}
		return out
	case string:
		if token := strings.TrimSpace(v); token != "" {
			return []ImportItem{{AccessToken: token}}
		}
		return nil
	case map[string]any:
		item := ImportItem{
			AccessToken:  firstNonEmpty(stringValue(v["access_token"]), stringValue(v["accessToken"])),
			RefreshToken: firstNonEmpty(stringValue(v["refresh_token"]), stringValue(v["refreshToken"])),
			IDToken:      firstNonEmpty(stringValue(v["id_token"]), stringValue(v["idToken"])),
			Type:         firstNonEmpty(stringValue(v["type"]), stringValue(v["plan_type"])),
			SourceType:   firstNonEmpty(stringValue(v["source_type"]), stringValue(v["export_type"])),
			Email:        stringValue(v["email"]),
			UserID:       firstNonEmpty(stringValue(v["user_id"]), stringValue(v["account_id"])),
			Proxy:        stringValue(v["proxy"]),
			Quota:        intValue(v["quota"]),
			RestoreAt:    stringValue(v["restore_at"]),
		}
		if item.AccessToken == "" {
			var out []ImportItem
			for _, key := range []string{"accounts", "items", "data", "result", "account"} {
				if nested, ok := v[key]; ok {
					out = append(out, collectImportItems(nested)...)
				}
			}
			if tokens, ok := v["tokens"].([]any); ok {
				for _, token := range tokens {
					if text, ok := token.(string); ok && strings.TrimSpace(text) != "" {
						out = append(out, ImportItem{AccessToken: strings.TrimSpace(text)})
					}
				}
			}
			return out
		}
		return []ImportItem{item}
	default:
		return nil
	}
}

func (s *Service) Delete(ctx context.Context, ids []string) (int, error) {
	removed := 0
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if err := s.store.DeleteAccount(ctx, id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *Service) Update(ctx context.Context, id string, updates map[string]any) (store.Account, error) {
	a, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return store.Account{}, err
	}
	if value, ok := updates["status"].(string); ok && value != "" {
		a.Status = value
	}
	if value, ok := updates["proxy"].(string); ok {
		a.Proxy = strings.TrimSpace(value)
	}
	if value, ok := updates["quota"].(float64); ok {
		a.Quota = int(value)
	}
	if value, ok := updates["type"].(string); ok && value != "" {
		a.Type = value
	}
	if err := s.store.UpdateAccount(ctx, a); err != nil {
		return store.Account{}, err
	}
	return s.store.GetAccount(ctx, id)
}

func (s *Service) Export(ctx context.Context, ids []string) ([]map[string]any, error) {
	items, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []map[string]any
	for _, a := range items {
		if len(wanted) > 0 && !wanted[a.ID] {
			continue
		}
		out = append(out, map[string]any{
			"type": a.Type, "email": a.Email, "account_id": a.UserID,
			"access_token": a.AccessToken, "refresh_token": a.RefreshToken, "id_token": a.IDToken,
			"source_type": a.SourceType, "quota": a.Quota, "expired": "",
		})
	}
	return out, nil
}

func (s *Service) RefreshOne(ctx context.Context, id string) error {
	a, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	wasDisabled := a.Status == store.AccountDisabled
	a, err = s.EnsureFreshToken(ctx, a)
	if err != nil {
		if !wasDisabled && s.config.Snapshot().Image.AutoRemoveInvalidAccounts && isInvalidCredentialError(err) {
			_ = s.store.DeleteAccount(ctx, a.ID)
			s.store.AddAccountEvent(ctx, a.ID, "account_removed_invalid", err.Error())
			return err
		}
		if !wasDisabled {
			a.Status = store.AccountError
		}
		a.LastRefreshError = err.Error()
		_ = s.store.UpdateAccount(ctx, a)
		s.store.AddAccountEvent(ctx, a.ID, "token_refresh_failed", err.Error())
		return err
	}
	info, err := s.fetchInfo(ctx, a)
	if err != nil && isHTTPStatus(err, http.StatusUnauthorized) && strings.TrimSpace(a.RefreshToken) != "" {
		if refreshed, refreshErr := s.ForceRefreshToken(ctx, a); refreshErr == nil {
			a = refreshed
			info, err = s.fetchInfo(ctx, a)
		}
	}
	if err != nil {
		if !wasDisabled && s.config.Snapshot().Image.AutoRemoveInvalidAccounts && isInvalidCredentialError(err) {
			_ = s.store.DeleteAccount(ctx, a.ID)
			s.store.AddAccountEvent(ctx, a.ID, "account_removed_invalid", err.Error())
			return err
		}
		if !wasDisabled {
			a.Status = store.AccountError
		}
		a.LastRefreshError = err.Error()
		_ = s.store.UpdateAccount(ctx, a)
		s.store.AddAccountEvent(ctx, a.ID, "refresh_failed", err.Error())
		return err
	}
	a.Email = firstNonEmpty(info.Email, a.Email)
	a.UserID = firstNonEmpty(info.UserID, a.UserID)
	a.Type = firstNonEmpty(info.Type, a.Type)
	a.Quota = info.Quota
	a.RestoreAt = info.RestoreAt
	a.LastRefreshError = ""
	if !wasDisabled {
		a.Status = store.AccountNormal
		if info.Quota <= 0 {
			a.Status = store.AccountLimited
		}
	}
	if err := s.store.UpdateAccount(ctx, a); err != nil {
		return err
	}
	s.store.AddAccountEvent(ctx, a.ID, "refreshed", fmt.Sprintf("type=%s quota=%d", a.Type, a.Quota))
	return nil
}

// EnsureFreshToken refreshes an access token when it is near expiry. OAuth
// refresh rotates the access token in place, while the stable account ID keeps
// jobs and logs attached to the same account.
func (s *Service) EnsureFreshToken(ctx context.Context, a store.Account) (store.Account, error) {
	return s.refreshTokenGrant(ctx, a, false)
}

func (s *Service) ForceRefreshToken(ctx context.Context, a store.Account) (store.Account, error) {
	return s.refreshTokenGrant(ctx, a, true)
}

func (s *Service) refreshTokenGrant(ctx context.Context, a store.Account, force bool) (nextAccount store.Account, err error) {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return a, nil
	}
	defer func() {
		if err == nil {
			return
		}
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		latest, getErr := s.store.GetAccount(updateCtx, a.ID)
		if getErr != nil {
			return
		}
		latest.LastRefreshError = "token refresh: " + truncate(err.Error(), 300)
		_ = s.store.UpdateAccount(updateCtx, latest)
	}()
	if !force {
		if seconds := jwtRemainingSeconds(a.AccessToken); seconds > 24*60*60 && !refreshTokenKeepaliveDue(a) {
			return a, nil
		}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", a.RefreshToken)
	form.Set("client_id", "app_2SKx67EdpoN0G6j64rFvigXD")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return a, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36")
	resp, err := s.clientFor(a.Proxy).Do(req)
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return a, fmt.Errorf("oauth refresh HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&data); err != nil {
		return a, err
	}
	next := a
	if v := stringValue(data["access_token"]); v != "" {
		next.AccessToken = v
	}
	if v := stringValue(data["refresh_token"]); v != "" {
		next.RefreshToken = v
	}
	if v := stringValue(data["id_token"]); v != "" {
		next.IDToken = v
	}
	if next.AccessToken == a.AccessToken {
		return a, errors.New("oauth refresh returned no access token")
	}
	next.LastTokenRefreshAt = time.Now().UTC().Format(time.RFC3339)
	next.LastRefreshError = ""
	if err := s.store.UpdateAccount(ctx, next); err != nil {
		return a, err
	}
	s.store.AddAccountEvent(ctx, a.ID, "access_token_refreshed", "refresh_token grant")
	return next, nil
}

func refreshTokenKeepaliveDue(a store.Account) bool {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return false
	}
	last := strings.TrimSpace(a.LastTokenRefreshAt)
	if last == "" {
		if payload := decodeJWTPayload(a.AccessToken); payload != nil {
			switch value := payload["iat"].(type) {
			case float64:
				return time.Since(time.Unix(int64(value), 0)) >= 7*24*time.Hour
			case json.Number:
				if issued, parseErr := value.Int64(); parseErr == nil {
					return time.Since(time.Unix(issued, 0)) >= 7*24*time.Hour
				}
			}
		}
		last = a.CreatedAt
	}
	parsed, parseErr := time.Parse(time.RFC3339, last)
	if parseErr != nil {
		return false
	}
	return time.Since(parsed) >= 7*24*time.Hour
}

func isHTTPStatus(err error, status int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == status
}

func isInvalidCredentialError(err error) bool {
	if isHTTPStatus(err, http.StatusUnauthorized) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "token_invalidated") ||
		strings.Contains(text, "token revoked") ||
		strings.Contains(text, "invalidated oauth token") ||
		strings.Contains(text, "unauthorized")
}

func (s *Service) RefreshMany(ctx context.Context, ids []string) RefreshResult {
	result := RefreshResult{Items: []store.Account{}}
	if len(ids) == 0 {
		accounts, _ := s.store.ListAccounts(ctx)
		for _, a := range accounts {
			if a.Status == store.AccountDisabled {
				continue
			}
			ids = append(ids, a.ID)
		}
	}
	workers := s.config.Snapshot().Performance.AccountRefreshWorkers
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if err := s.RefreshOne(ctx, id); err != nil {
					mu.Lock()
					result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", id, err))
					mu.Unlock()
				} else {
					mu.Lock()
					result.Refreshed++
					mu.Unlock()
				}
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	result.Items, _ = s.store.ListAccounts(ctx)
	return result
}

func (s *Service) Acquire(ctx context.Context) (store.Account, func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	restoreChecked := false
	for {
		cfg := s.config.Snapshot()
		accounts, err := s.store.ListAccounts(ctx)
		if err != nil {
			return store.Account{}, nil, err
		}
		if !restoreChecked {
			restoreChecked = true
			if s.refreshDueLimited(ctx, accounts) > 0 {
				accounts, err = s.store.ListAccounts(ctx)
				if err != nil {
					return store.Account{}, nil, err
				}
			}
		}
		s.mu.Lock()
		var candidate *store.Account
		for i := range accounts {
			a := &accounts[i]
			if a.Status != store.AccountNormal || a.Quota <= 0 {
				continue
			}
			if s.active[a.ID] >= cfg.Image.AccountConcurrency {
				continue
			}
			if candidate == nil ||
				a.LastUsedAt < candidate.LastUsedAt ||
				(a.LastUsedAt == candidate.LastUsedAt && a.Quota > candidate.Quota) ||
				(a.LastUsedAt == candidate.LastUsedAt && a.Quota == candidate.Quota && a.ID < candidate.ID) {
				candidate = a
			}
		}
		if candidate != nil {
			s.active[candidate.ID]++
			chosen := *candidate
			s.mu.Unlock()
			release := func() {
				s.mu.Lock()
				if s.active[chosen.ID] <= 1 {
					delete(s.active, chosen.ID)
				} else {
					s.active[chosen.ID]--
				}
				s.mu.Unlock()
			}
			return chosen, release, nil
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return store.Account{}, nil, errors.New("no available image quota")
		}
		select {
		case <-ctx.Done():
			return store.Account{}, nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) refreshDueLimited(ctx context.Context, accounts []store.Account) int {
	s.refreshDueMu.Lock()
	defer s.refreshDueMu.Unlock()
	nowValue := time.Now().UTC()
	refreshed := 0
	for _, item := range accounts {
		if item.Status != store.AccountLimited || !restoreTimeReached(item.RestoreAt, nowValue) {
			continue
		}
		if err := s.RefreshOne(ctx, item.ID); err != nil {
			continue
		}
		refreshed++
		if refreshed >= 5 {
			break
		}
	}
	return refreshed
}

func restoreTimeReached(value string, nowValue time.Time) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006/01/02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return !parsed.After(nowValue)
		}
	}
	if unixValue, err := strconv.ParseInt(value, 10, 64); err == nil {
		if unixValue > 1_000_000_000_000 {
			unixValue /= 1000
		}
		return !time.Unix(unixValue, 0).After(nowValue)
	}
	return false
}

func (s *Service) MarkResult(ctx context.Context, accountID string, success bool) {
	a, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	if success {
		a.SuccessCount++
		if a.Quota > 0 {
			a.Quota--
		}
		if a.Quota == 0 && a.Status == store.AccountNormal {
			a.Status = store.AccountLimited
		}
	} else {
		a.FailCount++
	}
	a.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	_ = s.store.UpdateAccount(ctx, a)
}

func (s *Service) MarkLimited(ctx context.Context, accountID, reason string) {
	a, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	if a.Status == store.AccountDisabled {
		return
	}
	a.Status = store.AccountLimited
	a.LastRefreshError = truncate(reason, 300)
	a.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	_ = s.store.UpdateAccount(ctx, a)
	s.store.AddAccountEvent(ctx, a.ID, "rate_limited", reason)
}

func (s *Service) MarkCredentialError(ctx context.Context, accountID, reason string) {
	a, err := s.store.GetAccount(ctx, accountID)
	if err != nil || a.Status == store.AccountDisabled {
		return
	}
	if s.config.Snapshot().Image.AutoRemoveInvalidAccounts {
		_ = s.store.DeleteAccount(ctx, a.ID)
		s.store.AddAccountEvent(ctx, a.ID, "account_removed_invalid", reason)
		return
	}
	a.Status = store.AccountError
	a.LastRefreshError = truncate(reason, 300)
	_ = s.store.UpdateAccount(ctx, a)
	s.store.AddAccountEvent(ctx, a.ID, "credential_error", reason)
}

type remoteInfo struct {
	Email     string
	UserID    string
	Type      string
	Quota     int
	RestoreAt string
}

func (s *Service) fetchInfo(ctx context.Context, a store.Account) (remoteInfo, error) {
	client := s.clientFor(a.Proxy)
	var me map[string]any
	if err := s.getJSON(ctx, client, a, http.MethodGet, "/backend-api/me", nil, &me); err != nil {
		return remoteInfo{}, err
	}
	var initPayload map[string]any
	if err := s.getJSON(ctx, client, a, http.MethodPost, "/backend-api/conversation/init", map[string]any{
		"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": -480,
	}, &initPayload); err != nil {
		return remoteInfo{}, err
	}
	var accountPayload map[string]any
	if err := s.getJSON(ctx, client, a, http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480", nil, &accountPayload); err != nil {
		return remoteInfo{}, err
	}
	info := remoteInfo{}
	info.Email = stringValue(me["email"])
	info.UserID = stringValue(me["id"])
	accounts, _ := accountPayload["accounts"].(map[string]any)
	defaultAccount, _ := accounts["default"].(map[string]any)
	acc, _ := defaultAccount["account"].(map[string]any)
	info.Type = firstNonEmpty(stringValue(acc["plan_type"]), "free")
	limits, _ := initPayload["limits_progress"].([]any)
	for _, raw := range limits {
		item, ok := raw.(map[string]any)
		if !ok || stringValue(item["feature_name"]) != "image_gen" {
			continue
		}
		info.Quota = intValue(item["remaining"])
		info.RestoreAt = stringValue(item["reset_after"])
		break
	}
	return info, nil
}

func (s *Service) getJSON(ctx context.Context, client *http.Client, a store.Account, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return err
	}
	setHeaders(req, a.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &HTTPError{Status: resp.StatusCode, Path: path, Body: string(data)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func (s *Service) clientFor(proxy string) *http.Client {
	key := strings.TrimSpace(proxy)
	if v, ok := s.clients.Load(key); ok {
		return v.(*http.Client)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 8
	transport.MaxIdleConnsPerHost = 2
	if key != "" {
		if parsed, err := url.Parse(key); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	} else if global := strings.TrimSpace(s.config.Snapshot().ProxyURL); global != "" {
		if parsed, err := url.Parse(global); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	actual, _ := s.clients.LoadOrStore(key, client)
	return actual.(*http.Client)
}

func setHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("X-OpenAI-Target-Path", req.URL.Path)
	req.Header.Set("X-OpenAI-Target-Route", req.URL.Path)
}

func fillFromJWT(a store.Account) store.Account {
	payload := decodeJWTPayload(a.AccessToken)
	if payload == nil {
		return a
	}
	if a.Email == "" {
		if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
			if a.UserID == "" {
				a.UserID = stringValue(auth["chatgpt_account_id"])
			}
		}
		if profile, ok := payload["https://api.openai.com/profile"].(map[string]any); ok {
			a.Email = stringValue(profile["email"])
		}
	}
	if a.Email == "" {
		a.Email = stringValue(payload["email"])
	}
	if a.Type == "" {
		a.Type = "free"
	}
	if a.SourceType == "" {
		a.SourceType = "web"
	}
	return a
}

func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func jwtRemainingSeconds(token string) int64 {
	payload := decodeJWTPayload(token)
	if payload == nil {
		return 0
	}
	var exp int64
	switch v := payload["exp"].(type) {
	case float64:
		exp = int64(v)
	case json.Number:
		exp, _ = v.Int64()
	}
	return exp - time.Now().Unix()
}

func publicAccounts(items []store.Account) []store.Account {
	out := make([]store.Account, 0, len(items))
	for _, item := range items {
		out = append(out, item.Public())
	}
	return out
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n] + "…"
}
