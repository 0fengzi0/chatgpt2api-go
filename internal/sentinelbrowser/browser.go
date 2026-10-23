package sentinelbrowser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

const (
	defaultChatGPTURL      = "https://chatgpt.com/"
	defaultPinnedAsset     = "/unauth-mweb/assets/turnstile-DykPLDEM.js"
	defaultPageRefresh     = 30 * time.Minute
	defaultNavigateTimeout = 60 * time.Second
	defaultSolveTimeout    = 120 * time.Second
)

var challengeTitles = []string{"请稍候", "Just a moment", "Attention Required"}
var browserVersionPattern = regexp.MustCompile(`(?:Edg|Chrome|Chromium)/([0-9]+)`)

// Options controls the single browser instance used by the solver.
type Options struct {
	BrowserPath      string
	ProxyURL         string
	UserAgent        string
	Timezone         string
	Locale           string
	Headless         bool
	NoSandbox        bool
	RefreshInterval  time.Duration
	NavigateTimeout  time.Duration
	SolveTimeout     time.Duration
	ChatGPTURL       string
	ExtraBrowserArgs []string
}

// TurnstileRequest contains challenge material only. It never includes an
// account token, cookie, or user prompt.
type TurnstileRequest struct {
	P            string         `json:"p"`
	DX           string         `json:"dx"`
	Requirements map[string]any `json:"requirements"`
}

// Status is safe to expose through the local health endpoint.
type Status struct {
	Ready            bool   `json:"ready"`
	PageAgeSeconds   int64  `json:"page_age_seconds"`
	BrowserVersion   string `json:"browser_version"`
	BrowserPath      string `json:"browser_path"`
	UserAgent        string `json:"user_agent"`
	Timezone         string `json:"timezone"`
	Headless         bool   `json:"headless"`
	LastError        string `json:"last_error,omitempty"`
	TurnstileAsset   string `json:"turnstile_asset,omitempty"`
	RefreshInterval  int64  `json:"refresh_interval_seconds"`
	SolveTimeoutSecs int64  `json:"solve_timeout_seconds"`
}

// Manager owns one browser, one page, and one serialized solve operation.
type Manager struct {
	opts Options

	mu             sync.Mutex
	baseCtx        context.Context
	cancelBase     context.CancelFunc
	browser        *rod.Browser
	page           *rod.Page
	browserPath    string
	browserVersion string
	userAgent      string
	turnstileAsset string
	openedAt       time.Time
	lastError      string
}

func New(opts Options) *Manager {
	if opts.ChatGPTURL == "" {
		opts.ChatGPTURL = defaultChatGPTURL
	}
	if opts.Locale == "" {
		opts.Locale = "zh-CN"
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = defaultPageRefresh
	}
	if opts.NavigateTimeout <= 0 {
		opts.NavigateTimeout = defaultNavigateTimeout
	}
	if opts.SolveTimeout <= 0 {
		opts.SolveTimeout = defaultSolveTimeout
	}
	baseCtx, cancelBase := context.WithCancel(context.Background())
	return &Manager{opts: opts, baseCtx: baseCtx, cancelBase: cancelBase}
}

// Start launches the browser and waits until chatgpt.com is ready.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openLocked(ctx)
}

// Close releases the browser and all of its child processes.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.closeLocked()
	if m.cancelBase != nil {
		m.cancelBase()
		m.cancelBase = nil
	}
	return err
}

// SetProxyURL applies a new proxy to the next browser page. Closing the old
// page prevents an account request from continuing through stale proxy state.
func (m *Manager) SetProxyURL(value string) {
	value = strings.TrimSpace(value)
	m.mu.Lock()
	defer m.mu.Unlock()
	if value == strings.TrimSpace(m.opts.ProxyURL) {
		return
	}
	m.opts.ProxyURL = value
	_ = m.closeLocked()
}

// UserAgent returns the actual UA of the page that will calculate Turnstile.
// The main service uses this value when it creates the Sentinel p token.
func (m *Manager) UserAgent(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensurePageLocked(ctx); err != nil {
		return "", err
	}
	value, err := m.page.Context(ctx).Timeout(m.opts.SolveTimeout).Eval(`() => navigator.userAgent`)
	if err != nil {
		return "", fmt.Errorf("read browser user agent: %w", err)
	}
	userAgent := strings.TrimSpace(value.Value.Str())
	if userAgent == "" {
		return "", errors.New("browser returned an empty user agent")
	}
	m.userAgent = userAgent
	return userAgent, nil
}

// SolveTurnstile is the provider-facing adapter.
func (m *Manager) SolveTurnstile(ctx context.Context, dx, p string, requirements map[string]any) (string, error) {
	return m.Solve(ctx, TurnstileRequest{P: p, DX: dx, Requirements: requirements})
}

// Solve calculates the Turnstile enforcement token inside the live ChatGPT page.
func (m *Manager) Solve(ctx context.Context, request TurnstileRequest) (string, error) {
	if strings.TrimSpace(request.P) == "" {
		return "", errors.New("turnstile p token is required")
	}
	if request.Requirements == nil {
		return "", errors.New("full chat requirements are required for the browser solver")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	solveCtx, cancel := context.WithTimeout(ctx, m.opts.SolveTimeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.ensurePageLocked(solveCtx); err != nil {
			lastErr = err
		} else {
			asset, err := m.turnstileAssetLocked(solveCtx)
			if err != nil {
				lastErr = err
			} else {
				token, err := m.solveSDKLocked(solveCtx, request, asset)
				if err == nil && token != "" {
					m.lastError = ""
					return token, nil
				}
				if err != nil {
					lastErr = err
				} else {
					lastErr = errors.New("turnstile module returned an empty token")
				}
			}
		}
		m.lastError = errorString(lastErr)
		m.turnstileAsset = ""
		if attempt == 0 {
			if err := m.openLocked(solveCtx); err != nil {
				lastErr = fmt.Errorf("%v; reopen failed: %w", lastErr, err)
				break
			}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("turnstile solve failed")
	}
	return "", lastErr
}

// Status reports the current browser state without evaluating page JavaScript.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		Ready:            pageOpen(m.page) && time.Since(m.openedAt) < m.opts.RefreshInterval,
		BrowserVersion:   m.browserVersion,
		BrowserPath:      m.browserPath,
		UserAgent:        m.userAgent,
		Timezone:         m.opts.Timezone,
		Headless:         m.opts.Headless,
		LastError:        m.lastError,
		TurnstileAsset:   m.turnstileAsset,
		RefreshInterval:  int64(m.opts.RefreshInterval / time.Second),
		SolveTimeoutSecs: int64(m.opts.SolveTimeout / time.Second),
	}
	if !m.openedAt.IsZero() {
		status.PageAgeSeconds = int64(time.Since(m.openedAt) / time.Second)
	}
	return status
}

func (m *Manager) ensurePageLocked(ctx context.Context) error {
	if pageOpen(m.page) && time.Since(m.openedAt) < m.opts.RefreshInterval {
		ready, err := m.pageReadyLocked(ctx)
		if err == nil && ready {
			return nil
		}
		if err != nil {
			m.lastError = err.Error()
		}
	}
	return m.openLocked(ctx)
}

func (m *Manager) openLocked(ctx context.Context) error {
	_ = m.closeLocked()

	browserPath, err := ResolveBrowserPath(m.opts.BrowserPath)
	if err != nil {
		return err
	}
	launcherHandle := launcher.New().
		Bin(browserPath).
		HeadlessNew(m.opts.Headless).
		NoSandbox(m.opts.NoSandbox).
		Set("disable-gpu").
		Set("disable-component-update").
		Set("disable-domain-reliability").
		Set("disable-extensions").
		Set("disable-background-networking").
		Set("disable-sync").
		Set("disable-search-engine-choice-screen").
		Set("hide-scrollbars").
		Set("mute-audio").
		Set("no-service-autorun").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("renderer-process-limit", "1").
		Set("js-flags", "--max-old-space-size=128").
		Set("lang", m.opts.Locale)
	launcherHandle.Delete("enable-automation")
	disableFeatures := append(
		[]string{},
		launcherHandle.Flags[flags.Flag("disable-features")]...,
	)
	disableFeatures = append(disableFeatures,
		"msEdgeSearchIndexer",
		"OptimizationHints",
		"MediaRouter",
		"DialMediaRouteProvider",
		"GlobalMediaControls",
		"AutofillServerCommunication",
		"InterestFeedContentSuggestions",
		"CalculateNativeWinOcclusion",
	)
	launcherHandle.Set("disable-features", disableFeatures...)
	if runtime.GOOS != "windows" {
		launcherHandle.Set("disable-dev-shm-usage")
	}
	if m.opts.ProxyURL != "" {
		launcherHandle.Proxy(m.opts.ProxyURL)
	}
	if m.opts.UserAgent != "" {
		launcherHandle.Set("user-agent", m.opts.UserAgent)
	}
	for _, arg := range m.opts.ExtraBrowserArgs {
		name, value, hasValue := strings.Cut(arg, "=")
		name = strings.TrimLeft(strings.TrimSpace(name), "-")
		if name == "" {
			continue
		}
		if hasValue {
			launcherHandle.Set(flags.Flag(name), value)
		} else {
			launcherHandle.Set(flags.Flag(name))
		}
	}

	controlURL, err := launcherHandle.Launch()
	if err != nil {
		return fmt.Errorf("launch browser %s: %w", browserPath, err)
	}
	browser := rod.New().ControlURL(controlURL).Context(m.baseCtx)
	if err := browser.Connect(); err != nil {
		launcherHandle.Kill()
		return fmt.Errorf("connect browser: %w", err)
	}
	version, err := browser.Version()
	if err != nil {
		_ = browser.Close()
		launcherHandle.Kill()
		return fmt.Errorf("read browser version: %w", err)
	}

	page, err := stealth.Page(browser)
	if err != nil {
		_ = browser.Close()
		launcherHandle.Kill()
		return fmt.Errorf("create stealth page: %w", err)
	}
	userAgent, platform := browserIdentity(m.opts.UserAgent, version.Product)
	if err := page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent:      userAgent,
		AcceptLanguage: m.opts.Locale,
		Platform:       platform,
	}); err != nil {
		_ = browser.Close()
		launcherHandle.Kill()
		return fmt.Errorf("set browser user agent: %w", err)
	}
	m.userAgent = userAgent
	if m.opts.Locale != "" {
		if err := (proto.EmulationSetLocaleOverride{Locale: m.opts.Locale}).Call(page.Context(ctx).Timeout(5 * time.Second)); err != nil {
			_ = browser.Close()
			launcherHandle.Kill()
			return fmt.Errorf("set browser locale: %w", err)
		}
	}
	if m.opts.Timezone != "" {
		if err := (proto.EmulationSetTimezoneOverride{TimezoneID: m.opts.Timezone}).Call(page.Context(ctx).Timeout(5 * time.Second)); err != nil {
			_ = browser.Close()
			launcherHandle.Kill()
			return fmt.Errorf("set browser timezone: %w", err)
		}
	}

	m.browser = browser
	m.page = page
	m.browserPath = browserPath
	m.browserVersion = version.Product
	m.openedAt = time.Now()

	navigatePage := page.Context(ctx).Timeout(m.opts.NavigateTimeout)
	if err := navigatePage.Navigate(m.opts.ChatGPTURL); err != nil {
		_ = m.closeLocked()
		return fmt.Errorf("navigate to chatgpt.com: %w", err)
	}
	if err := navigatePage.WaitLoad(); err != nil {
		_ = m.closeLocked()
		return fmt.Errorf("wait for chatgpt.com load: %w", err)
	}

	deadline := time.Now().Add(m.opts.NavigateTimeout)
	for {
		if ready, err := m.pageReadyLocked(ctx); err == nil && ready {
			uaValue, uaErr := page.Context(ctx).Timeout(m.opts.SolveTimeout).Eval(`() => navigator.userAgent`)
			if uaErr == nil && strings.TrimSpace(uaValue.Value.Str()) != "" {
				m.userAgent = strings.TrimSpace(uaValue.Value.Str())
			}
			m.lastError = ""
			return nil
		}
		if time.Now().After(deadline) {
			titleValue, _ := page.Context(ctx).Timeout(5 * time.Second).Eval(`() => document.title`)
			title := titleValue.Value.Str()
			_ = m.closeLocked()
			return fmt.Errorf("chatgpt.com page not ready (title=%q), likely stuck on a Cloudflare challenge", title)
		}
		select {
		case <-ctx.Done():
			_ = m.closeLocked()
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (m *Manager) closeLocked() error {
	var errs []error
	if m.page != nil {
		if err := m.page.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if m.browser != nil {
		if err := m.browser.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.page = nil
	m.browser = nil
	m.openedAt = time.Time{}
	m.turnstileAsset = ""
	return errors.Join(errs...)
}

func (m *Manager) pageReadyLocked(ctx context.Context) (bool, error) {
	if !pageOpen(m.page) {
		return false, errors.New("browser page is closed")
	}
	page := m.page.Context(ctx).Timeout(5 * time.Second)
	titleValue, err := page.Eval(`() => document.title`)
	if err != nil {
		return false, err
	}
	title := titleValue.Value.Str()
	for _, marker := range challengeTitles {
		if strings.Contains(title, marker) {
			return false, nil
		}
	}
	hostValue, err := page.Eval(`() => location.hostname`)
	if err != nil {
		return false, err
	}
	if hostValue.Value.Str() != "chatgpt.com" {
		return false, nil
	}
	readyValue, err := page.Eval(`() => document.readyState`)
	if err != nil {
		return false, err
	}
	return readyValue.Value.Str() == "complete" || readyValue.Value.Str() == "interactive", nil
}

func (m *Manager) turnstileAssetLocked(ctx context.Context) (string, error) {
	if m.turnstileAsset != "" {
		return m.turnstileAsset, nil
	}
	value, err := m.page.Context(ctx).Timeout(m.opts.SolveTimeout).Eval(discoverAssetScript)
	if err != nil {
		return "", fmt.Errorf("discover turnstile asset: %w", err)
	}
	asset := strings.TrimSpace(value.Value.Str())
	if asset == "" {
		asset = defaultPinnedAsset
	}
	m.turnstileAsset = asset
	return asset, nil
}

func (m *Manager) solveSDKLocked(ctx context.Context, request TurnstileRequest, asset string) (string, error) {
	value, err := m.page.Context(ctx).Timeout(m.opts.SolveTimeout).Eval(
		solveTurnstileScript,
		request.Requirements,
		request.P,
		asset,
	)
	if err != nil {
		return "", fmt.Errorf("evaluate turnstile module: %w", err)
	}
	result := strings.TrimSpace(value.Value.Str())
	if strings.HasPrefix(result, "ERR:") {
		return "", errors.New(strings.TrimPrefix(result, "ERR:"))
	}
	return result, nil
}

// ResolveBrowserPath finds a full Chromium-family browser executable.
func ResolveBrowserPath(configured string) (string, error) {
	if value := strings.TrimSpace(configured); value != "" {
		return validateBrowserPath(value)
	}
	if value := strings.TrimSpace(os.Getenv("SENTINEL_BROWSER_BIN")); value != "" {
		return validateBrowserPath(value)
	}

	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
	case "darwin":
		candidates = []string{
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		candidates = []string{
			"/usr/bin/microsoft-edge",
			"/usr/bin/microsoft-edge-stable",
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/usr/lib/chromium/chromium",
			"/opt/google/chrome/chrome",
			"/opt/microsoft/msedge/msedge",
		}
	}
	for _, candidate := range candidates {
		if resolved, err := validateBrowserPath(candidate); err == nil {
			return resolved, nil
		}
	}
	if resolved, ok := launcher.LookPath(); ok {
		return validateBrowserPath(resolved)
	}
	return "", errors.New("Chromium-family browser not found; install Chromium/Chrome/Edge or set SENTINEL_BROWSER_BIN")
}

func validateBrowserPath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve browser path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("browser executable %q: %w", absolute, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("browser executable %q is a directory", absolute)
	}
	return absolute, nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func pageOpen(page *rod.Page) bool {
	return page != nil && page.GetContext().Err() == nil
}

func browserIdentity(configuredUserAgent, product string) (string, string) {
	platform := "Win32"
	osPart := "Windows NT 10.0; Win64; x64"
	switch runtime.GOOS {
	case "darwin":
		platform = "MacIntel"
		osPart = "Macintosh; Intel Mac OS X 10_15_7"
	case "linux":
		platform = "Linux x86_64"
		osPart = "X11; Linux x86_64"
	}

	if value := strings.TrimSpace(configuredUserAgent); value != "" {
		return value, platform
	}
	major := "155"
	if match := browserVersionPattern.FindStringSubmatch(product); len(match) > 1 {
		major = match[1]
	}
	ua := fmt.Sprintf(
		"Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36",
		osPart,
		major,
	)
	if strings.Contains(product, "Edg/") {
		ua += " Edg/" + major + ".0.0.0"
	}
	return ua, platform
}

const discoverAssetScript = `
async () => {
  const pattern = /\/turnstile-[^/"']+\.js/;
  const seen = [];
  const push = (url) => {
    if (!url || seen.includes(url)) return;
    try {
      const parsed = new URL(url, location.href);
      if (pattern.test(parsed.pathname)) seen.push(parsed.pathname);
    } catch (err) {}
  };
  try { performance.getEntriesByType('resource').forEach((entry) => push(entry.name)); } catch (err) {}
  try { Array.from(document.scripts).forEach((script) => push(script.src)); } catch (err) {}
  if (seen.length) return seen[0];
  for (const script of Array.from(document.scripts)) {
    if (!script.src) continue;
    try {
      const text = await (await fetch(script.src)).text();
      const match = text.match(pattern);
      if (match) return match[0];
    } catch (err) {}
  }
  return '';
}
`

const solveTurnstileScript = `
async (requirements, key, asset) => {
  try {
    const mod = await import(asset);
    const turnstile = mod.turnstile || (mod.default && mod.default.turnstile);
    if (!turnstile || typeof turnstile.getEnforcementToken !== 'function') {
      return 'ERR:getEnforcementToken missing';
    }
    const original = WeakMap.prototype.get;
    WeakMap.prototype.get = function (target) {
      return target === requirements ? key : original.call(this, target);
    };
    try {
      return String(await turnstile.getEnforcementToken(requirements));
    } finally {
      WeakMap.prototype.get = original;
    }
  } catch (error) {
    return 'ERR:' + String(error).slice(0, 500);
  }
}
`
