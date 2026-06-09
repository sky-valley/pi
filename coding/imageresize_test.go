package coding

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func TestResizeDownscalesOversizedImage(t *testing.T) {
	// 3000x2400 PNG exceeds the 2000px cap → must be downscaled.
	img := image.NewRGBA(image.Rect(0, 0, 3000, 2400))
	for y := 0; y < 2400; y++ {
		for x := 0; x < 3000; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, mime, ok := resizeImageForModel(buf.Bytes(), "image/png")
	if !ok {
		t.Fatal("expected resize to succeed")
	}
	dec, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	b := dec.Bounds()
	if b.Dx() > imgMaxWidth || b.Dy() > imgMaxHeight {
		t.Fatalf("not downscaled: %dx%d", b.Dx(), b.Dy())
	}
	// aspect ratio preserved (3000:2400 = 5:4)
	if r := float64(b.Dx()) / float64(b.Dy()); r < 1.2 || r > 1.3 {
		t.Fatalf("aspect ratio not preserved: %dx%d", b.Dx(), b.Dy())
	}
	_ = mime
}

func TestSmallImagePassesThroughUnchanged(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	in := buf.Bytes()
	out, mime, ok := resizeImageForModel(in, "image/png")
	if !ok || mime != "image/png" {
		t.Fatalf("small image should pass through: ok=%v mime=%s", ok, mime)
	}
	if !bytes.Equal(out, in) {
		t.Fatal("small image should be returned unchanged")
	}
}

func TestExifOrientationApplied(t *testing.T) {
	// Stored 2400x1200 (landscape, oversized → forces the resize path where pi
	// bakes orientation), dark, with a BRIGHT square in the top-left. EXIF
	// orientation 6 means "rotate 90° CW to display": the top-left must end up
	// TOP-RIGHT. This verifies the rotation *direction*, not just the dimension
	// swap (a wrong-way rotation would also swap dims but place the marker wrong).
	const sw, sh = 2400, 1200
	img := image.NewRGBA(image.Rect(0, 0, sw, sh))
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			c := color.RGBA{R: 10, G: 10, B: 10, A: 255}
			if x < sw/4 && y < sh/3 { // top-left marker
				c = color.RGBA{R: 250, G: 250, B: 250, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var jbuf bytes.Buffer
	jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: 95})
	withExif := injectExifOrientation(jbuf.Bytes(), 6)

	if o := jpegOrientation(withExif); o != 6 {
		t.Fatalf("orientation not parsed: got %d", o)
	}
	resized, ok := resizeImage(withExif, "image/jpeg")
	if !ok {
		t.Fatal("resize failed")
	}
	if !resized.WasResized {
		t.Fatal("expected oversized oriented image to be resized (orientation baked)")
	}
	dec, _, _ := image.Decode(bytes.NewReader(resized.Data))
	b := dec.Bounds()
	if b.Dx() >= b.Dy() {
		t.Fatalf("orientation 6 should produce a portrait image, got %dx%d", b.Dx(), b.Dy())
	}
	bright := func(x, y int) uint32 { r, _, _, _ := dec.At(b.Min.X+x, b.Min.Y+y).RGBA(); return r >> 8 }
	topRight := bright(b.Dx()-b.Dx()/8, b.Dy()/12) // where the marker MUST be after a CW rotation
	topLeft := bright(b.Dx()/8, b.Dy()/12)         // where it must NOT be
	if topRight < 180 {
		t.Fatalf("marker not in top-right after orientation 6 (got brightness %d) — wrong rotation direction", topRight)
	}
	if topLeft > 120 {
		t.Fatalf("marker leaked into top-left (brightness %d) — wrong rotation", topLeft)
	}
}

// TestSmallOrientedImagePassesThrough verifies pi's contract: a small oriented
// JPEG is returned unchanged (EXIF intact, not baked), with post-orientation
// dimensions reported and wasResized=false.
func TestSmallOrientedImagePassesThrough(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	var jbuf bytes.Buffer
	jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: 95})
	withExif := injectExifOrientation(jbuf.Bytes(), 6)

	r, ok := resizeImage(withExif, "image/jpeg")
	if !ok {
		t.Fatal("resize failed")
	}
	if r.WasResized {
		t.Fatal("small image should not be resized")
	}
	if !bytes.Equal(r.Data, withExif) {
		t.Fatal("small image should be returned with original bytes unchanged (pi contract)")
	}
	// Orientation 6 swaps dimensions: stored 40x20 → reported 20x40.
	if r.Width != 20 || r.Height != 40 {
		t.Fatalf("expected post-orientation dims 20x40, got %dx%d", r.Width, r.Height)
	}
}

// TestReadToolResizesImage exercises the read tool end-to-end on an oversized image.
func TestReadToolResizesImage(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 2500, 100))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	os.WriteFile(filepath.Join(dir, "big.png"), buf.Bytes(), 0o644)

	r, err := readTool(dir).Execute(context.Background(), "id", map[string]any{"path": "big.png"}, func(agent.AgentToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	var imgContent *ai.ImageContent
	var text string
	for _, c := range r.Content {
		switch v := c.(type) {
		case ai.ImageContent:
			ic := v
			imgContent = &ic
		case ai.TextContent:
			text = v.Text
		}
	}
	if imgContent == nil {
		t.Fatalf("no image content returned; text=%q", text)
	}
	if imgContent.Data == "" {
		t.Fatal("empty image data")
	}
}

// injectExifOrientation inserts a minimal Exif APP1 segment with the given
// orientation right after the JPEG SOI marker.
func injectExifOrientation(jpegData []byte, orientation int) []byte {
	// TIFF header (little-endian) + 1 IFD entry for Orientation (0x0112, SHORT).
	tiff := []byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00} // header, IFD at offset 8
	tiff = append(tiff, 0x01, 0x00)                              // 1 entry
	tiff = append(tiff,
		0x12, 0x01, // tag 0x0112
		0x03, 0x00, // type SHORT
		0x01, 0x00, 0x00, 0x00, // count 1
		byte(orientation), 0x00, 0x00, 0x00, // value
	)
	tiff = append(tiff, 0x00, 0x00, 0x00, 0x00) // next IFD offset = 0

	exif := append([]byte("Exif\x00\x00"), tiff...)
	segLen := len(exif) + 2
	app1 := []byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen)}
	app1 = append(app1, exif...)

	out := append([]byte{0xFF, 0xD8}, app1...) // SOI + APP1
	return append(out, jpegData[2:]...)        // rest of original (after its SOI)
}
