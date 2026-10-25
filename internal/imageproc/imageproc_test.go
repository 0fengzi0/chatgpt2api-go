package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestConvertOutputFormats(t *testing.T) {
	source := testImagePNG(t)
	for _, format := range []string{"png", "jpeg", "webp"} {
		data, mime, width, height, err := Convert(source, format, 72, 16_000_000, "auto")
		if err != nil {
			t.Fatalf("convert %s: %v", format, err)
		}
		if len(data) == 0 || width != 4 || height != 4 {
			t.Fatalf("unexpected %s output: %dx%d bytes=%d", format, width, height, len(data))
		}
		if mime == "" {
			t.Fatalf("empty MIME for %s", format)
		}
		if _, detected, err := image.Decode(bytes.NewReader(data)); err != nil || detected != format {
			t.Fatalf("decode %s: detected=%q err=%v", format, detected, err)
		}
	}
}

func TestCompositeMaskMatchesDimensions(t *testing.T) {
	source := testImagePNG(t)
	mask := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 2; x++ {
			mask.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, mask); err != nil {
		t.Fatal(err)
	}
	merged, err := CompositeMask(source, buffer.Bytes(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(merged))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, alpha := decoded.At(0, 0).RGBA(); alpha == 0 {
		t.Fatal("masked area should be opaque")
	}
	if _, _, _, alpha := decoded.At(3, 0).RGBA(); alpha != 0 {
		t.Fatal("unmasked area should be transparent")
	}
}

func TestValidateUploadRejectsOversizedPixels(t *testing.T) {
	source := testImagePNG(t)
	if _, err := ValidateUpload(source, 1_000_000, 8); err == nil {
		t.Fatal("expected pixel limit error")
	}
}

func testImagePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 30, B: 40, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
