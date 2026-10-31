package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/account"
	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
	_ "github.com/gen2brain/webp"
)

const imageSystemHint = "picture_v2"

var (
	fileServiceRe = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	sedimentRe    = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
	fileIDRe      = regexp.MustCompile(`\bfile_[A-Za-z0-9_-]+\b`)
	convIDRe      = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)
)

type ChatGPT struct {
	accounts *account.Service
	config   *config.Store
	solver   TurnstileSolver
}

type TurnstileSolver interface {
	UserAgent(context.Context) (string, error)
	SolveTurnstile(context.Context, string, string, map[string]any) (string, error)
}

func NewChatGPT(accounts *account.Service, cfg *config.Store, solver TurnstileSolver) *ChatGPT {
	return &ChatGPT{
		accounts: accounts,
		config:   cfg,
		solver:   solver,
	}
}

type chatSession struct {
	baseURL       string
	token         string
	client        *http.Client
	userAgent     string
	deviceID      string
	sessionID     string
	scriptSources []string
	dataBuild     string
	solver        TurnstileSolver
	mu            sync.Mutex
	conversation  string
	fileIDs       []string
	sedimentIDs   []string
	text          strings.Builder
}

type requirements struct {
	Token      string
	ProofToken string
	Turnstile  string
	SO         string
}

func (p *ChatGPT) Generate(ctx context.Context, req Request) (Result, error) {
	result, err := p.generateOnce(ctx, req)
	if err == nil || !isTokenInvalidError(err.Error()) || ctx.Err() != nil {
		return result, err
	}
	return p.generateOnce(ctx, req)
}

func (p *ChatGPT) generateOnce(ctx context.Context, req Request) (out Result, retErr error) {
	acct, release, err := p.accounts.Acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	acct, err = p.accounts.EnsureFreshToken(ctx, acct)
	if err != nil {
		return Result{}, err
	}
	success := false
	defer func() {
		markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if retErr != nil && isTokenInvalidError(retErr.Error()) {
			if strings.TrimSpace(acct.RefreshToken) == "" {
				p.accounts.MarkCredentialError(markCtx, acct.ID, "access token is invalid and no refresh token is available")
			} else if _, refreshErr := p.accounts.ForceRefreshToken(markCtx, acct); refreshErr != nil {
				p.accounts.MarkCredentialError(markCtx, acct.ID, refreshErr.Error())
			}
		}
		if retErr != nil && isRateLimitError(retErr.Error()) {
			p.accounts.MarkLimited(markCtx, acct.ID, retErr.Error())
		}
		p.accounts.MarkResult(markCtx, acct.ID, success)
	}()
	cfg := p.config.Snapshot()
	sess, err := newChatSession(cfg, acct, p.solver)
	if err != nil {
		return Result{}, err
	}
	if req.Progress != nil {
		req.Progress("uploading")
	}
	uploaded := make([]map[string]any, 0, len(req.Images))
	excluded := make(map[string]struct{})
	for i, data := range req.Images {
		ref, err := sess.uploadImage(ctx, data, fmt.Sprintf("image_%d%s", i+1, uploadExtension(data)))
		if err != nil {
			return Result{}, err
		}
		uploaded = append(uploaded, ref)
		if id := stringValue(ref["file_id"]); id != "" {
			excluded[id] = struct{}{}
		}
	}
	if req.Progress != nil {
		req.Progress("bootstrapping")
	}
	if err := sess.bootstrap(ctx); err != nil {
		return Result{}, err
	}
	if req.Progress != nil {
		req.Progress("getting_token")
	}
	requ, err := sess.getRequirements(ctx)
	if err != nil {
		return Result{}, err
	}
	upstreamModel, err := resolveImageModel(req.Model, cfg.DefaultUpstreamModel)
	if err != nil {
		return Result{}, err
	}
	if req.Progress != nil {
		req.Progress("preparing_conversation")
	}
	upstreamPrompt := buildImagePrompt(req.Prompt, req.Size, req.Quality)
	if strings.EqualFold(strings.TrimSpace(req.Background), "transparent") {
		upstreamPrompt += "\n\nUse a genuinely transparent background and preserve an alpha channel where the output format supports it."
	}
	conduit, err := sess.prepareConversation(ctx, upstreamPrompt, requ, upstreamModel, cfg)
	if err != nil {
		return Result{}, err
	}
	if req.Progress != nil {
		req.Progress("starting_generation")
	}
	response, err := sess.startGeneration(ctx, upstreamPrompt, requ, conduit, upstreamModel, uploaded, cfg)
	if err != nil {
		return Result{}, err
	}
	if req.Progress != nil {
		req.Progress("generating")
	}
	streamErr := sess.consumeSSE(response, cfg.Image.PollTimeoutSeconds)
	_ = response.Body.Close()
	if streamErr != nil && !errors.Is(streamErr, io.EOF) {
		return Result{}, streamErr
	}
	if req.Progress != nil {
		req.Progress("resolving_image")
	}
	deadline := time.Now().Add(time.Duration(cfg.Image.PollTimeoutSeconds) * time.Second)
	for {
		if len(sess.fileIDs) > 0 || len(sess.sedimentIDs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			if strings.TrimSpace(sess.text.String()) != "" {
				if isPolicyText(sess.text.String()) {
					return Result{}, errors.New(sess.text.String())
				}
				return Result{}, errors.New("upstream did not return image assets")
			}
			return Result{}, errors.New("image generation timed out")
		}
		if sess.conversation == "" {
			time.Sleep(time.Second)
			continue
		}
		if err := sess.pollConversation(ctx, excluded); err == nil {
			if len(sess.fileIDs) > 0 || len(sess.sedimentIDs) > 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(max(1, cfg.Image.PollIntervalSeconds/3)) * time.Second):
		}
	}
	urls, err := sess.resolveURLs(ctx, sess.conversation, sess.fileIDs, sess.sedimentIDs)
	if err != nil {
		return Result{}, err
	}
	if len(urls) == 0 {
		return Result{}, errors.New("no downloadable image URL")
	}
	result := Result{
		MIME:          "image/png",
		RevisedPrompt: req.Prompt,
		AccountID:     acct.ID,
		AccountEmail:  acct.Email,
	}
	for _, imageURL := range urls {
		data, err := sess.download(ctx, imageURL, cfg.Image.MaxImageBytes)
		if err != nil {
			continue
		}
		if len(data) > 0 {
			result.Images = append(result.Images, data)
		}
	}
	if len(result.Images) == 0 {
		return Result{}, errors.New("failed to download generated images")
	}
	success = true
	return result, nil
}

func isTokenInvalidError(message string) bool {
	text := strings.ToLower(message)
	return strings.Contains(text, "http 401") ||
		strings.Contains(text, "token_invalidated") ||
		strings.Contains(text, "token revoked") ||
		strings.Contains(text, "invalidated oauth token")
}

func isRateLimitError(message string) bool {
	text := strings.ToLower(message)
	return strings.Contains(text, "http 429") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "rate_limit") ||
		strings.Contains(text, "rate limit")
}

func uploadExtension(data []byte) string {
	switch detectImageMIME(data) {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func detectImageMIME(data []byte) string {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return http.DetectContentType(data)
}

func buildImagePrompt(prompt, size, quality string) string {
	prompt = strings.TrimSpace(prompt)
	hints := ""
	if size = strings.TrimSpace(size); size != "" && !strings.EqualFold(size, "auto") {
		hints += "输出图片尺寸为 " + size + "。"
	}
	if strings.TrimSpace(quality) != "" {
		hints += "输出图片质量为 " + strings.TrimSpace(quality) + "。"
	}
	if hints == "" {
		return prompt
	}
	return prompt + "\n\n" + hints
}

func resolveImageModel(requestModel, configuredUpstream string) (string, error) {
	switch strings.TrimSpace(requestModel) {
	case "", "gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare":
	default:
		return "", fmt.Errorf("unsupported image model %q", requestModel)
	}
	upstream := strings.TrimSpace(configuredUpstream)
	if upstream == "" {
		upstream = "gpt-5-5"
	}
	return upstream, nil
}

func newChatSession(cfg config.Config, acct store.Account, solver TurnstileSolver) (*chatSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 8
	transport.MaxIdleConnsPerHost = 2
	proxy := strings.TrimSpace(acct.Proxy)
	if proxy == "" {
		proxy = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxy != "" {
		if parsed, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &chatSession{
		baseURL:       strings.TrimRight(cfg.ProviderBaseURL, "/"),
		token:         acct.AccessToken,
		client:        &http.Client{Transport: transport, Jar: jar},
		userAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0",
		deviceID:      uuid(),
		sessionID:     uuid(),
		scriptSources: []string{defaultPoWScript},
		solver:        solver,
	}, nil
}

func (s *chatSession) baseHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", s.userAgent)
	h.Set("Origin", s.baseURL)
	h.Set("Referer", s.baseURL+"/")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Sec-Ch-Ua", `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("OAI-Device-Id", s.deviceID)
	h.Set("OAI-Session-Id", s.sessionID)
	h.Set("OAI-Language", "zh-CN")
	h.Set("OAI-Client-Version", "prod-bede35f9dcd856d080e012478f0c1031faa2588e")
	h.Set("OAI-Client-Build-Number", "6631702")
	if s.token != "" {
		h.Set("Authorization", "Bearer "+s.token)
	}
	return h
}

func (s *chatSession) newRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header = s.baseHeaders()
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-OpenAI-Target-Path", path)
	req.Header.Set("X-OpenAI-Target-Route", path)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func (s *chatSession) bootstrap(ctx context.Context) error {
	req, err := s.newRequest(ctx, http.MethodGet, "/", nil, "")
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bootstrap HTTP %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	s.scriptSources, s.dataBuild = parsePoWResources(string(data))
	return nil
}

func (s *chatSession) getRequirements(ctx context.Context) (requirements, error) {
	requirementsUserAgent := s.userAgent
	if s.solver != nil {
		if solverUserAgent, err := s.solver.UserAgent(ctx); err == nil && solverUserAgent != "" {
			requirementsUserAgent = solverUserAgent
		} else if err != nil {
			return requirements{}, fmt.Errorf("sentinel solver user agent: %w", err)
		}
	}
	pToken := buildRequirementsToken(requirementsUserAgent, s.scriptSources, s.dataBuild)
	prepareBody, _ := json.Marshal(map[string]any{"p": pToken})
	req, err := s.newRequest(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", bytes.NewReader(prepareBody), "application/json")
	if err != nil {
		return requirements{}, err
	}
	var prepare map[string]any
	if err := s.doJSON(req, &prepare); err != nil {
		return requirements{}, err
	}
	proof := ""
	if pow, ok := prepare["proofofwork"].(map[string]any); ok && boolValue(pow["required"]) {
		proof, err = buildProofToken(stringValue(pow["seed"]), stringValue(pow["difficulty"]), s.userAgent, s.scriptSources, s.dataBuild)
		if err != nil {
			return requirements{}, err
		}
	}
	turnstile := ""
	if ts, ok := prepare["turnstile"].(map[string]any); ok && boolValue(ts["required"]) {
		if s.solver == nil {
			return requirements{}, errors.New("embedded sentinel solver is not configured")
		}
		dx := stringValue(ts["dx"])
		turnstile, err = s.solver.SolveTurnstile(ctx, dx, pToken, prepare)
		if err != nil {
			return requirements{}, fmt.Errorf("sentinel turnstile: %w", err)
		}
	}
	finalizeBody, _ := json.Marshal(map[string]any{
		"prepare_token":   stringValue(prepare["prepare_token"]),
		"proof_token":     proof,
		"turnstile_token": turnstile,
	})
	req, err = s.newRequest(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", bytes.NewReader(finalizeBody), "application/json")
	if err != nil {
		return requirements{}, err
	}
	var final map[string]any
	if err := s.doJSON(req, &final); err != nil {
		return requirements{}, err
	}
	requ := requirements{
		Token:      stringValue(final["token"]),
		ProofToken: proof,
		Turnstile:  turnstile,
		SO:         stringValue(final["so_token"]),
	}
	if requ.Token == "" {
		return requirements{}, errors.New("upstream returned empty chat requirements token")
	}
	return requ, nil
}

func (s *chatSession) prepareConversation(ctx context.Context, prompt string, requ requirements, model string, cfg config.Config) (string, error) {
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     uuid(),
		"model":                 model,
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []string{imageSystemHint},
		"partial_query": map[string]any{
			"id": uuid(), "author": map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []string{prompt}},
		},
		"supports_buffering":     true,
		"supported_encodings":    []string{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}
	if cfg.DefaultThinkingEffort != "" && cfg.DefaultThinkingEffort != "auto" {
		payload["thinking_effort"] = cfg.DefaultThinkingEffort
	}
	body, _ := json.Marshal(payload)
	req, err := s.newRequest(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	s.setRequirementsHeaders(req, requ)
	var out map[string]any
	if err := s.doJSON(req, &out); err != nil {
		return "", err
	}
	conduit := stringValue(out["conduit_token"])
	if conduit == "" {
		return "", errors.New("upstream did not return conduit token")
	}
	return conduit, nil
}

func (s *chatSession) startGeneration(ctx context.Context, prompt string, requ requirements, conduit, model string, uploaded []map[string]any, cfg config.Config) (*http.Response, error) {
	parts := make([]any, 0, len(uploaded)+1)
	for _, item := range uploaded {
		parts = append(parts, map[string]any{
			"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + stringValue(item["file_id"]),
			"width": intValue(item["width"]), "height": intValue(item["height"]), "size_bytes": intValue(item["file_size"]),
		})
	}
	parts = append(parts, prompt)
	content := map[string]any{"content_type": "text", "parts": []string{prompt}}
	metadata := map[string]any{
		"developer_mode_connector_ids": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false,
		"system_hints": []string{imageSystemHint}, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(uploaded) > 0 {
		content = map[string]any{"content_type": "multimodal_text", "parts": parts}
		attachments := make([]any, 0, len(uploaded))
		for _, item := range uploaded {
			attachments = append(attachments, map[string]any{
				"id": stringValue(item["file_id"]), "mimeType": stringValue(item["mime_type"]),
				"name": stringValue(item["file_name"]), "size": intValue(item["file_size"]),
				"width": intValue(item["width"]), "height": intValue(item["height"]),
			})
		}
		metadata["attachments"] = attachments
	}
	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id": uuid(), "author": map[string]any{"role": "user"}, "create_time": time.Now().Unix(),
			"content": content, "metadata": metadata,
		}},
		"parent_message_id": uuid(), "model": model, "client_prepare_state": "sent", "timezone_offset_min": -480,
		"timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true, "system_hints": []string{imageSystemHint}, "supports_buffering": true,
		"supported_encodings":                  []string{"v1"},
		"client_contextual_info":               map[string]any{"is_dark_mode": false, "time_since_loaded": 1200, "page_height": 1072, "page_width": 1724, "pixel_ratio": 1.2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"},
		"paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto",
	}
	if cfg.DefaultThinkingEffort != "" && cfg.DefaultThinkingEffort != "auto" {
		payload["thinking_effort"] = cfg.DefaultThinkingEffort
	}
	body, _ := json.Marshal(payload)
	req, err := s.newRequest(ctx, http.MethodPost, "/backend-api/f/conversation", bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Conduit-Token", conduit)
	s.setRequirementsHeaders(req, requ)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("image stream HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return resp, nil
}

func (s *chatSession) setRequirementsHeaders(req *http.Request, requ requirements) {
	req.Header.Set("OpenAI-Sentinel-Chat-Requirements-Token", requ.Token)
	if requ.ProofToken != "" {
		req.Header.Set("OpenAI-Sentinel-Proof-Token", requ.ProofToken)
	}
	if requ.Turnstile != "" {
		req.Header.Set("OpenAI-Sentinel-Turnstile-Token", requ.Turnstile)
	}
	if requ.SO != "" {
		req.Header.Set("OpenAI-Sentinel-SO-Token", requ.SO)
	}
}

func (s *chatSession) consumeSSE(resp *http.Response, timeoutSecs int) error {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var value any
		if json.Unmarshal([]byte(payload), &value) != nil {
			continue
		}
		s.absorbJSON(value)
	}
	return scanner.Err()
}

func (s *chatSession) pollConversation(ctx context.Context, excluded map[string]struct{}) error {
	req, err := s.newRequest(ctx, http.MethodGet, "/backend-api/conversation/"+s.conversation, nil, "")
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("poll HTTP %d", resp.StatusCode)
	}
	var value any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&value); err != nil {
		return err
	}
	s.absorbJSONWithExcluded(value, excluded)
	return nil
}

func (s *chatSession) absorbJSON(value any) {
	s.absorbJSONWithExcluded(value, nil)
}

func (s *chatSession) absorbJSONWithExcluded(value any, excluded map[string]struct{}) {
	switch v := value.(type) {
	case string:
		s.absorbText(v, excluded)
	case []any:
		for _, item := range v {
			s.absorbJSONWithExcluded(item, excluded)
		}
	case map[string]any:
		if cid := stringValue(v["conversation_id"]); cid != "" {
			s.conversation = cid
		}
		for _, item := range v {
			s.absorbJSONWithExcluded(item, excluded)
		}
	}
}

func (s *chatSession) absorbText(text string, excluded map[string]struct{}) {
	if strings.TrimSpace(text) != "" {
		s.text.WriteString(text)
		s.text.WriteString("\n")
	}
	if s.conversation == "" {
		if m := convIDRe.FindStringSubmatch(text); len(m) > 1 {
			s.conversation = m[1]
		}
	}
	for _, id := range fileServiceRe.FindAllStringSubmatch(text, -1) {
		if len(id) > 1 && !contains(s.fileIDs, id[1]) && !excludedID(excluded, id[1]) {
			s.fileIDs = append(s.fileIDs, id[1])
		}
	}
	for _, id := range sedimentRe.FindAllStringSubmatch(text, -1) {
		if len(id) > 1 && !contains(s.sedimentIDs, id[1]) {
			s.sedimentIDs = append(s.sedimentIDs, id[1])
		}
	}
	for _, id := range fileIDRe.FindAllString(text, -1) {
		if !contains(s.fileIDs, id) && !excludedID(excluded, id) {
			s.fileIDs = append(s.fileIDs, id)
		}
	}
}

func (s *chatSession) resolveURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error) {
	var urls []string
	seen := map[string]bool{}
	for _, id := range fileIDs {
		path := "/backend-api/files/" + url.PathEscape(id) + "/download"
		req, _ := s.newRequest(ctx, http.MethodGet, path, nil, "")
		var out map[string]any
		if err := s.doJSON(req, &out); err != nil {
			continue
		}
		if u := firstNonEmpty(stringValue(out["download_url"]), stringValue(out["url"])); u != "" && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	for _, id := range sedimentIDs {
		if conversationID == "" {
			continue
		}
		path := "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(id) + "/download"
		req, _ := s.newRequest(ctx, http.MethodGet, path, nil, "")
		var out map[string]any
		if err := s.doJSON(req, &out); err != nil {
			continue
		}
		if u := firstNonEmpty(stringValue(out["download_url"]), stringValue(out["url"])); u != "" && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls, nil
}

func (s *chatSession) download(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = s.baseHeaders()
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("image download HTTP %d", resp.StatusCode)
	}
	reader := io.Reader(resp.Body)
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, errors.New("image exceeds max bytes")
	}
	return data, nil
}

func (s *chatSession) uploadImage(ctx context.Context, data []byte, name string) (map[string]any, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode uploaded image: %w", err)
	}
	mime := detectImageMIME(data)
	body, _ := json.Marshal(map[string]any{
		"file_name": name, "file_size": len(data), "use_case": "multimodal", "width": cfg.Width, "height": cfg.Height,
	})
	req, err := s.newRequest(ctx, http.MethodPost, "/backend-api/files", bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	if err := s.doJSON(req, &meta); err != nil {
		return nil, err
	}
	uploadURL := stringValue(meta["upload_url"])
	fileID := stringValue(meta["file_id"])
	if uploadURL == "" || fileID == "" {
		return nil, errors.New("invalid upload response")
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	put.Header.Set("Content-Type", mime)
	put.Header.Set("x-ms-blob-type", "BlockBlob")
	put.Header.Set("x-ms-version", "2020-04-08")
	put.Header.Set("Origin", s.baseURL)
	put.Header.Set("Referer", s.baseURL+"/")
	resp, err := s.client.Do(put)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upload HTTP %d", resp.StatusCode)
	}
	req, _ = s.newRequest(ctx, http.MethodPost, "/backend-api/files/"+url.PathEscape(fileID)+"/uploaded", strings.NewReader("{}"), "application/json")
	if err := s.doJSON(req, nil); err != nil {
		return nil, err
	}
	return map[string]any{
		"file_id": fileID, "file_name": name, "file_size": len(data), "mime_type": mime,
		"width": cfg.Width, "height": cfg.Height,
	}, nil
}

func (s *chatSession) doJSON(req *http.Request, out any) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("upstream %s HTTP %d: %s", req.URL.Path, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func isPolicyText(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range []string{"content policy", "moderation", "blocked", "不能生成", "无法生成", "违反", "内容政策"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func excludedID(excluded map[string]struct{}, id string) bool {
	if excluded == nil {
		return false
	}
	_, ok := excluded[id]
	return ok
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}
