package provider

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestProofTokenCanBeBuilt(t *testing.T) {
	token, err := buildProofToken("seed", "00", "test-agent", []string{defaultPoWScript}, "test-build")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "gAAAAAB") {
		t.Fatalf("unexpected token prefix: %s", token)
	}
	encoded := strings.TrimPrefix(token, "gAAAAAB")
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		t.Fatalf("token payload is not base64: %v", err)
	}
}

func TestBuildImagePromptIncludesSizeAndQuality(t *testing.T) {
	got := buildImagePrompt("draw a cat", "1536x1024", "high")
	want := "draw a cat\n\n输出图片尺寸为 1536x1024。输出图片质量为 high。"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

func TestBuildImagePromptSkipsAutoSize(t *testing.T) {
	got := buildImagePrompt("keep the source ratio", "auto", "high")
	want := "keep the source ratio\n\n输出图片质量为 high。"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}
