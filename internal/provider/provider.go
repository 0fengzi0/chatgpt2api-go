package provider

import (
	"context"
	"encoding/json"
	"math"
	"strings"
)

type Request struct {
	Prompt   string
	Model    string
	Size     string
	Quality  string
	Images   [][]byte
	Progress func(string)
}

type Result struct {
	Images        [][]byte
	MIME          string
	RevisedPrompt string
}

type Provider interface {
	Generate(ctx context.Context, req Request) (Result, error)
}

func max(a, b int) int {
	return int(math.Max(float64(a), float64(b)))
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
