package imageproc

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strings"

	"github.com/gen2brain/webp"
)

type Info struct {
	Format string
	Width  int
	Height int
}

func ValidateUpload(data []byte, maxBytes, maxPixels int64) (Info, error) {
	if len(data) == 0 {
		return Info{}, errors.New("image file is empty")
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return Info{}, fmt.Errorf("image file exceeds %d bytes", maxBytes)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Info{}, errors.New("invalid or unsupported image")
	}
	switch strings.ToLower(format) {
	case "png", "jpeg", "webp":
	default:
		return Info{}, fmt.Errorf("unsupported image format %q; use png, jpeg, or webp", format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Info{}, errors.New("image dimensions are invalid")
	}
	if maxPixels > 0 && int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return Info{}, fmt.Errorf("image exceeds maximum pixel count")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return Info{}, errors.New("image data is incomplete or corrupt")
	}
	return Info{Format: strings.ToLower(format), Width: cfg.Width, Height: cfg.Height}, nil
}

func CompositeMask(input, mask []byte, maxPixels int64) ([]byte, error) {
	if len(mask) == 0 {
		return input, nil
	}
	source, _, err := image.Decode(bytes.NewReader(input))
	if err != nil {
		return nil, fmt.Errorf("decode source image: %w", err)
	}
	if maxPixels > 0 && int64(source.Bounds().Dx())*int64(source.Bounds().Dy()) > maxPixels {
		return nil, errors.New("source image exceeds maximum pixel count")
	}
	maskImage, maskFormat, err := image.Decode(bytes.NewReader(mask))
	if err != nil {
		return nil, fmt.Errorf("decode mask image: %w", err)
	}
	if strings.ToLower(maskFormat) != "png" {
		return nil, errors.New("mask must be a PNG image")
	}
	if source.Bounds().Size() != maskImage.Bounds().Size() {
		return nil, errors.New("mask dimensions must match the image")
	}
	base := image.NewNRGBA(source.Bounds())
	draw.Draw(base, base.Bounds(), source, source.Bounds().Min, draw.Src)
	maskBounds := maskImage.Bounds()
	for y := 0; y < base.Bounds().Dy(); y++ {
		for x := 0; x < base.Bounds().Dx(); x++ {
			_, _, _, alpha := maskImage.At(maskBounds.Min.X+x, maskBounds.Min.Y+y).RGBA()
			offset := base.PixOffset(base.Bounds().Min.X+x, base.Bounds().Min.Y+y)
			base.Pix[offset+3] = uint8(alpha >> 8)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, base); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func Convert(data []byte, outputFormat string, compression int, maxPixels int64, background string) ([]byte, string, int, int, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("decode generated image: %w", err)
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	if maxPixels > 0 && int64(width)*int64(height) > maxPixels {
		return nil, "", 0, 0, errors.New("generated image exceeds maximum pixel count")
	}
	if strings.EqualFold(strings.TrimSpace(background), "opaque") {
		opaque := image.NewRGBA(source.Bounds())
		draw.Draw(opaque, opaque.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(opaque, opaque.Bounds(), source, source.Bounds().Min, draw.Over)
		source = opaque
	}
	var out bytes.Buffer
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "", "png":
		encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := encoder.Encode(&out, source); err != nil {
			return nil, "", 0, 0, err
		}
		return out.Bytes(), "image/png", width, height, nil
	case "jpeg":
		quality := compression
		if quality < 0 {
			quality = 90
		}
		opaque := image.NewRGBA(source.Bounds())
		draw.Draw(opaque, opaque.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(opaque, opaque.Bounds(), source, source.Bounds().Min, draw.Over)
		if err := jpeg.Encode(&out, opaque, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", 0, 0, err
		}
		return out.Bytes(), "image/jpeg", width, height, nil
	case "webp":
		quality := compression
		if quality < 0 {
			quality = 90
		}
		if err := webp.Encode(&out, source, webp.Options{Quality: quality, Method: 4}); err != nil {
			return nil, "", 0, 0, err
		}
		return out.Bytes(), "image/webp", width, height, nil
	default:
		return nil, "", 0, 0, fmt.Errorf("unsupported output_format %q", outputFormat)
	}
}

func Dimensions(data []byte) (int, int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

func EstimateUsage(prompt string, imageCount, width, height int) map[string]any {
	textTokens := (len([]rune(prompt)) + 3) / 4
	outputTokens := 0
	if imageCount > 0 {
		pixels := width * height
		if pixels <= 0 {
			pixels = 1024 * 1024
		}
		outputTokens = imageCount * max(272, pixels/38)
	}
	return map[string]any{
		"input_tokens": textTokens,
		"input_tokens_details": map[string]any{
			"image_tokens": 0,
			"text_tokens":  textTokens,
		},
		"output_tokens": outputTokens,
		"total_tokens":  textTokens + outputTokens,
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
