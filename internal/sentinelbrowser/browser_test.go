package sentinelbrowser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveBrowserPathConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "browser")
	if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveBrowserPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("resolved path = %q, want %q", resolved, path)
	}
}

func TestResolveBrowserPathRejectsDirectory(t *testing.T) {
	if _, err := ResolveBrowserPath(t.TempDir()); err == nil {
		t.Fatal("expected directory rejection")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	manager := New(Options{})
	if manager.opts.ChatGPTURL != defaultChatGPTURL {
		t.Fatalf("url = %q", manager.opts.ChatGPTURL)
	}
	if manager.opts.Locale != "zh-CN" {
		t.Fatalf("locale = %q", manager.opts.Locale)
	}
	if manager.opts.RefreshInterval != defaultPageRefresh {
		t.Fatalf("refresh interval = %s", manager.opts.RefreshInterval)
	}
	if runtime.GOOS == "" {
		t.Fatal("runtime GOOS is empty")
	}
}

func TestBrowserIdentityUsesBrowserVersion(t *testing.T) {
	userAgent, platform := browserIdentity("", "Edg/155.0.4283.62")
	if !strings.Contains(userAgent, "Chrome/155.0.0.0") {
		t.Fatalf("user agent does not contain Chrome major: %q", userAgent)
	}
	if !strings.Contains(userAgent, "Edg/155.0.0.0") {
		t.Fatalf("user agent does not contain Edge major: %q", userAgent)
	}
	if platform == "" {
		t.Fatal("platform is empty")
	}
}

func TestBrowserIdentityKeepsConfiguredUserAgent(t *testing.T) {
	const configured = "custom-user-agent"
	userAgent, _ := browserIdentity(configured, "Chrome/155.0.0.0")
	if userAgent != configured {
		t.Fatalf("user agent = %q, want %q", userAgent, configured)
	}
}
