package provider

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	mathrand "math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

const (
	defaultPoWScript = "https://chatgpt.com/backend-api/sentinel/sdk.js"
	powErrorPrefix   = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"
)

var scriptSrcRe = regexp.MustCompile(`<script[^>]+src=["']([^"']+)["']`)
var dataBuildRe = regexp.MustCompile(`data-build=["']([^"']+)["']`)

func parsePoWResources(html string) ([]string, string) {
	matches := scriptSrcRe.FindAllStringSubmatch(html, -1)
	var sources []string
	for _, m := range matches {
		if len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			sources = append(sources, m[1])
		}
	}
	if len(sources) == 0 {
		sources = []string{defaultPoWScript}
	}
	build := ""
	if m := dataBuildRe.FindStringSubmatch(html); len(m) > 1 {
		build = m[1]
	}
	return sources, build
}

func buildRequirementsToken(userAgent string, scriptSources []string, dataBuild string) string {
	data := buildPoWConfig(userAgent, scriptSources, dataBuild)
	data[3] = 1
	// The legacy token is just a browser-looking config payload.
	raw, _ := json.Marshal(data)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(raw)
}

func buildProofToken(seed, difficulty, userAgent string, scriptSources []string, dataBuild string) (string, error) {
	if strings.TrimSpace(seed) == "" || strings.TrimSpace(difficulty) == "" {
		return buildRequirementsToken(userAgent, scriptSources, dataBuild), nil
	}
	difficulty = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(difficulty)), "0x")
	target, err := hex.DecodeString(difficulty)
	if err != nil || len(target) == 0 {
		return "", fmt.Errorf("invalid proof difficulty")
	}
	data := buildPoWConfig(userAgent, scriptSources, dataBuild)
	// Precompute the JSON around the two changing fields. This mirrors the
	// browser SDK's compact JSON shape without encoding 500k full objects.
	head, _ := json.Marshal(data[:3])
	mid, _ := json.Marshal(data[4:9])
	tail, _ := json.Marshal(data[10:])
	headStr := strings.TrimSuffix(string(head), "]") + ","
	midStr := "," + strings.TrimPrefix(strings.TrimSuffix(string(mid), "]"), "[") + ","
	tailStr := "," + strings.TrimPrefix(string(tail), "[")
	seedBytes := []byte(seed)
	for i := 0; i < 500000; i++ {
		finalJSON := headStr + strconv.Itoa(i) + midStr + strconv.Itoa(i>>1) + tailStr
		encoded := base64.StdEncoding.EncodeToString([]byte(finalJSON))
		sum := sha3.Sum512(append(seedBytes, []byte(encoded)...))
		if len(target) <= len(sum) && bytesLessOrEqual(sum[:len(target)], target) {
			return "gAAAAAB" + encoded, nil
		}
	}
	fallback := powErrorPrefix + base64.StdEncoding.EncodeToString([]byte(strconv.Quote(seed)))
	return "gAAAAAB" + fallback, fmt.Errorf("failed to solve proof token")
}

func buildPoWConfig(userAgent string, scriptSources []string, dataBuild string) []any {
	source := defaultPoWScript
	if len(scriptSources) > 0 {
		source = scriptSources[mathrand.Intn(len(scriptSources))]
	}
	screen := [][2]int{{1920, 1080}, {1440, 900}, {2560, 1440}, {3840, 2160}}[mathrand.Intn(4)]
	perf := mathrand.Float64()*1000 + 1000
	now := time.Now().In(time.FixedZone("EST", -5*3600)).Format("Mon Jan _2 15:04:05 2006") + " GMT-0500 (Eastern Standard Time)"
	return []any{
		screen[0] + screen[1],
		now,
		int64(4294705152),
		1,
		userAgent,
		source,
		dataBuild,
		"en-US",
		"en-US,es-US,en,es",
		mathrand.Float64(),
		"hardwareConcurrency-32",
		"location",
		"window",
		perf,
		uuid(),
		"",
		[]int{8, 16, 24, 32}[mathrand.Intn(4)],
		float64(time.Now().UnixNano())/1e6 - perf,
		0, 0, 0, 0, 0, 0,
		0,
	}
}

func uuid() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func bytesLessOrEqual(a, b []byte) bool {
	for i := range a {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return true
}

func powConfigForTest() []any {
	return buildPoWConfig("test", []string{defaultPoWScript}, "test-build")
}

// Keep math import used in builds where future difficulty normalization needs
// floating point arithmetic.
var _ = math.Abs
