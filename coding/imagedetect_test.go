package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// A minimal valid 1x1 PNG (IHDR + IDAT + IEND).
func minimalPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // signature
		0x00, 0x00, 0x00, 0x0d, // IHDR length = 13
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1x1
		0x08, 0x06, 0x00, 0x00, 0x00, // bit depth/color/etc
		0x1f, 0x15, 0xc4, 0x89, // CRC
		0x00, 0x00, 0x00, 0x0a, // IDAT length
		0x49, 0x44, 0x41, 0x54, // "IDAT"
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, // data
		0x0d, 0x0a, 0x2d, 0xb4, // CRC
		0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82, // IEND
	}
}

// Animated PNG: IHDR (13) then an acTL chunk before IDAT.
func animatedPNG() []byte {
	out := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, // IHDR len 13
		0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00,
		0x1f, 0x15, 0xc4, 0x89,
		0x00, 0x00, 0x00, 0x08, // acTL len 8
		0x61, 0x63, 0x54, 0x4c, // "acTL"
		0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00,
		0xde, 0xad, 0xbe, 0xef, // CRC
	}
	return out
}

func TestDetectMimeRealPNG(t *testing.T) {
	if got := detectSupportedImageMimeType(minimalPNG()); got != "image/png" {
		t.Fatalf("expected image/png, got %q", got)
	}
}

func TestDetectMimeAnimatedPNGRejected(t *testing.T) {
	if got := detectSupportedImageMimeType(animatedPNG()); got != "" {
		t.Fatalf("animated PNG should be rejected, got %q", got)
	}
}

func TestDetectMimeNonIHDRPNGRejected(t *testing.T) {
	buf := minimalPNG()
	// Corrupt the IHDR chunk-type so it is no longer a valid PNG header.
	buf[12] = 'X'
	if got := detectSupportedImageMimeType(buf); got != "" {
		t.Fatalf("non-IHDR PNG should be rejected, got %q", got)
	}
}

func TestDetectMimeJPEG(t *testing.T) {
	if got := detectSupportedImageMimeType([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 0}); got != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", got)
	}
}

func TestDetectMimeCMYKJPEGRejected(t *testing.T) {
	// ffd8fff7 = CMYK / extended sequential JPEG → rejected.
	if got := detectSupportedImageMimeType([]byte{0xff, 0xd8, 0xff, 0xf7, 0, 0}); got != "" {
		t.Fatalf("CMYK JPEG should be rejected, got %q", got)
	}
}

func TestDetectMimeGIFWebp(t *testing.T) {
	if got := detectSupportedImageMimeType([]byte("GIF89a")); got != "image/gif" {
		t.Fatalf("expected image/gif, got %q", got)
	}
	webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBP")...)
	if got := detectSupportedImageMimeType(webp); got != "image/webp" {
		t.Fatalf("expected image/webp, got %q", got)
	}
}

// An extensionless real PNG file reads as an image (detection by content).
func TestReadExtensionlessImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "screenshot") // no extension
	os.WriteFile(p, minimalPNG(), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "screenshot"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "Read image file [image/png]") {
		t.Fatalf("extensionless PNG should read as image: %q", resultText(r))
	}
	hasImage := false
	for _, c := range r.Content {
		if _, ok := c.(ai.ImageContent); ok {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatalf("expected an image content block")
	}
}

// A .png-named file with animated-PNG content falls back to the text path (no image).
func TestReadAnimatedPNGFallsBackToText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "anim.png")
	os.WriteFile(p, animatedPNG(), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "anim.png"})
	if err != nil {
		t.Fatal(err)
	}
	// Should NOT be treated as an image: no "Read image file" note.
	if strings.Contains(resultText(r), "Read image file") {
		t.Fatalf("animated PNG should not be sent as image: %q", resultText(r))
	}
}

// A .png-named file with CMYK-JPEG content also falls back to text.
func TestReadCMYKMislabeledFallsBackToText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.png")
	os.WriteFile(p, []byte{0xff, 0xd8, 0xff, 0xf7, 0, 0, 0, 0}, 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "fake.png"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText(r), "Read image file") {
		t.Fatalf("CMYK JPEG should not be sent as image: %q", resultText(r))
	}
}
