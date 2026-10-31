package api

import "testing"

func TestSupportedImageSizePresets(t *testing.T) {
	ts := newTestServer(t)
	sizes := []string{
		"1024x1024",
		"1536x1024",
		"1024x1536",
		"1824x1024",
		"1024x1824",
		"1360x1024",
		"1024x1360",
		"2384x1024",
		"1024x2384",
		"2048x2048",
		"2160x1440",
		"1440x2160",
		"2560x1440",
		"1440x2560",
		"1920x1440",
		"1440x1920",
		"2048x1536",
		"2560x1920",
		"2732x2048",
		"3360x1440",
		"1440x3360",
		"3840x2160",
		"2160x3840",
	}
	for _, size := range sizes {
		t.Run(size, func(t *testing.T) {
			options, validationErr := ts.server.normalizeImageOptions(imageOptionsInput{
				Model:  "gpt-image-2",
				Prompt: "test image",
				Size:   size,
			})
			if validationErr != nil {
				t.Fatalf("size %s rejected: %s", size, validationErr.Message)
			}
			if options.Size != size {
				t.Fatalf("size = %q, want %q", options.Size, size)
			}
		})
	}
}
