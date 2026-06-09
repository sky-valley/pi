package coding

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"strconv"

	_ "golang.org/x/image/webp" // decode-only, to match photon's webp support
)

// Image post-processing for the read tool (port of pi's resizeImageInProcess in
// utils/image-resize-core.ts). Downscales images that exceed the inline limits
// before sending them to the model and applies EXIF orientation. The decision
// surface (target dimensions, format choice, wasResized) is a faithful port and
// is differentially tested against pi (see imageresize_parity_test.go). The
// pixel data itself is not byte-identical: pi uses Photon/Lanczos3 and Rust
// encoders, this uses a pure-Go bilinear resize and the std-lib encoders.

const (
	imgMaxWidth       = 2000
	imgMaxHeight      = 2000
	imgMaxBase64Bytes = int(4.5 * 1024 * 1024) // 4.5MB base64, headroom below Anthropic's 5MB
)

// Matches pi's qualitySteps = dedupe([jpegQuality(default 80), 85, 70, 55, 40]).
var jpegQualities = []int{80, 85, 70, 55, 40}

// ResizeResult mirrors the object pi's resizeImage returns.
type ResizeResult struct {
	Data           []byte // raw image bytes to send to the model (not base64)
	MimeType       string
	OriginalWidth  int
	OriginalHeight int
	Width          int
	Height         int
	WasResized     bool
}

// base64Size returns the encoded length of n bytes — ceil(n/3)*4, matching pi's
// `Math.ceil(inputBytes.byteLength / 3) * 4`.
func base64Size(n int) int { return ((n + 2) / 3) * 4 }

// jsRound mirrors JS Math.round (round half toward +Infinity) for non-negative x.
func jsRound(x float64) int { return int(math.Floor(x + 0.5)) }

// resizeImage is a faithful port of pi's resizeImageInProcess. It returns the
// decision result and true, or a zero result and false when the image cannot be
// brought under the byte limit (pi returns null).
func resizeImage(inputBytes []byte, mimeType string) (ResizeResult, bool) {
	inputB64 := base64Size(len(inputBytes))

	img, format, err := image.Decode(bytes.NewReader(inputBytes))
	if err != nil {
		return ResizeResult{}, false // pi: photon decode failure → null
	}

	// pi applies EXIF orientation to the working image (used for dimensions and
	// for the resized output), reading the orientation from the original bytes.
	oriented := applyExifOrientationFromBytes(img, inputBytes)
	ob := oriented.Bounds()
	ow, oh := ob.Dx(), ob.Dy()

	if mimeType == "" {
		mimeType = "image/" + format
	}

	// Already within all limits → return the ORIGINAL bytes unchanged (pi does
	// not bake orientation here; it reports the post-orientation dimensions and
	// relies on the model honoring EXIF). wasResized = false.
	if ow <= imgMaxWidth && oh <= imgMaxHeight && inputB64 < imgMaxBase64Bytes {
		return ResizeResult{
			Data: inputBytes, MimeType: mimeType,
			OriginalWidth: ow, OriginalHeight: oh,
			Width: ow, Height: oh, WasResized: false,
		}, true
	}

	// Initial target: scale to fit within max dimensions, preserving aspect.
	// pi uses Math.round for the dependent dimension.
	tw, th := ow, oh
	if tw > imgMaxWidth {
		th = jsRound(float64(th) * float64(imgMaxWidth) / float64(tw))
		tw = imgMaxWidth
	}
	if th > imgMaxHeight {
		tw = jsRound(float64(tw) * float64(imgMaxHeight) / float64(th))
		th = imgMaxHeight
	}

	// Shrink-and-encode loop: at each size try PNG then the JPEG quality steps,
	// taking the first candidate under the byte limit; otherwise scale down by
	// 0.75 (floored) until 1×1 (mirrors pi's while loop exactly).
	cw, ch := tw, th
	for {
		scaled := oriented
		if cw != ow || ch != oh {
			scaled = bilinearResize(oriented, cw, ch)
		}
		if enc, mime, fit := encodeUnderLimit(scaled); fit {
			return ResizeResult{
				Data: enc, MimeType: mime,
				OriginalWidth: ow, OriginalHeight: oh,
				Width: cw, Height: ch, WasResized: true,
			}, true
		}
		if cw == 1 && ch == 1 {
			break
		}
		nw, nh := cw, ch
		if cw != 1 {
			nw = max1(int(math.Floor(float64(cw) * 0.75)))
		}
		if ch != 1 {
			nh = max1(int(math.Floor(float64(ch) * 0.75)))
		}
		if nw == cw && nh == ch {
			break
		}
		cw, ch = nw, nh
	}
	return ResizeResult{}, false
}

// ResizeImageDecision exposes the image-pipeline decision (dimensions, format,
// wasResized) for differential-testing tools. It mirrors pi's resizeImage.
func ResizeImageDecision(data []byte, mimeType string) (ResizeResult, bool) {
	return resizeImage(data, mimeType)
}

// resizeImageForModel is a thin wrapper retained for the read tool: it returns
// the bytes + mime to embed, ok=false when the image can't be brought under the
// limit. (The richer decision is available via resizeImage.)
func resizeImageForModel(data []byte, mimeType string) (out []byte, outMime string, ok bool) {
	r, ok := resizeImage(data, mimeType)
	if !ok {
		return nil, "", false
	}
	return r.Data, r.MimeType, true
}

// formatDimensionNote mirrors pi's formatDimensionNote: a coordinate-mapping
// hint emitted only when the image was resized. The scale uses JS toFixed(2)
// semantics (two decimals, round half to even is NOT used — toFixed rounds
// half away from zero for the common cases here).
func formatDimensionNote(r ResizeResult) string {
	if !r.WasResized || r.Width == 0 {
		return ""
	}
	scale := float64(r.OriginalWidth) / float64(r.Width)
	return fmt.Sprintf("[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %s to map to original image.]",
		r.OriginalWidth, r.OriginalHeight, r.Width, r.Height, toFixed2(scale))
}

// toFixed2 formats x with exactly two decimals, matching JS Number.toFixed(2).
func toFixed2(x float64) string {
	return strconv.FormatFloat(x, 'f', 2, 64)
}

func encodeUnderLimit(img image.Image) ([]byte, string, bool) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err == nil && base64Size(buf.Len()) < imgMaxBase64Bytes {
		return append([]byte(nil), buf.Bytes()...), "image/png", true
	}
	for _, q := range jpegQualities {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err == nil && base64Size(buf.Len()) < imgMaxBase64Bytes {
			return append([]byte(nil), buf.Bytes()...), "image/jpeg", true
		}
	}
	return nil, "", false
}

// bilinearResize downscales src to tw×th using bilinear interpolation.
func bilinearResize(src image.Image, tw, th int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	if sw == 0 || sh == 0 {
		return dst
	}
	xRatio := float64(sw-1) / float64(max1(tw-1))
	yRatio := float64(sh-1) / float64(max1(th-1))
	for y := 0; y < th; y++ {
		fy := float64(y) * yRatio
		y0 := int(fy)
		dy := fy - float64(y0)
		for x := 0; x < tw; x++ {
			fx := float64(x) * xRatio
			x0 := int(fx)
			dx := fx - float64(x0)
			r00, g00, b00, a00 := at(src, sb.Min.X+x0, sb.Min.Y+y0)
			r10, g10, b10, a10 := at(src, sb.Min.X+x0+1, sb.Min.Y+y0)
			r01, g01, b01, a01 := at(src, sb.Min.X+x0, sb.Min.Y+y0+1)
			r11, g11, b11, a11 := at(src, sb.Min.X+x0+1, sb.Min.Y+y0+1)
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(lerp2(r00, r10, r01, r11, dx, dy)),
				G: uint8(lerp2(g00, g10, g01, g11, dx, dy)),
				B: uint8(lerp2(b00, b10, b01, b11, dx, dy)),
				A: uint8(lerp2(a00, a10, a01, a11, dx, dy)),
			})
		}
	}
	return dst
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func at(img image.Image, x, y int) (r, g, b, a uint32) {
	bb := img.Bounds()
	if x >= bb.Max.X {
		x = bb.Max.X - 1
	}
	if y >= bb.Max.Y {
		y = bb.Max.Y - 1
	}
	return img.At(x, y).RGBA()
}

func lerp2(c00, c10, c01, c11 uint32, dx, dy float64) uint32 {
	top := float64(c00>>8)*(1-dx) + float64(c10>>8)*dx
	bot := float64(c01>>8)*(1-dx) + float64(c11>>8)*dx
	return uint32(top*(1-dy) + bot*dy)
}

// applyExifOrientationFromBytes applies the EXIF orientation found in the
// original bytes (JPEG or WebP, matching pi's getExifOrientation) to img.
func applyExifOrientationFromBytes(img image.Image, data []byte) image.Image {
	o := exifOrientationFromBytes(data)
	if o <= 1 {
		return img
	}
	return applyOrientation(img, o)
}

// exifOrientationFromBytes reads the EXIF orientation (1-8) from JPEG or WebP
// bytes, mirroring pi's getExifOrientation. Returns 1 when absent.
func exifOrientationFromBytes(data []byte) int {
	switch {
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8: // JPEG
		return jpegOrientation(data)
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return webpOrientation(data)
	}
	return 1
}

// jpegOrientation extracts the EXIF orientation (1-8) from a JPEG, or 1 if absent.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			break
		}
		marker := data[i+1]
		if marker == 0xD9 || marker == 0xDA { // EOI or SOS
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			break
		}
		if marker == 0xE1 { // APP1
			seg := data[i+4 : i+2+segLen]
			if o, ok := exifOrientation(seg); ok {
				return o
			}
		}
		i += 2 + segLen
	}
	return 1
}

// webpOrientation reads orientation from a WebP EXIF chunk (mirrors pi's
// findWebpTiffOffset + readOrientationFromTiff).
func webpOrientation(data []byte) int {
	off := 12
	for off+8 <= len(data) {
		chunkID := string(data[off : off+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		dataStart := off + 8
		if chunkID == "EXIF" {
			if dataStart+chunkSize > len(data) {
				return 1
			}
			tiff := data[dataStart:]
			if chunkSize >= 6 && len(tiff) >= 6 && string(tiff[0:6]) == "Exif\x00\x00" {
				tiff = tiff[6:]
			}
			if o, ok := tiffOrientation(tiff); ok {
				return o
			}
			return 1
		}
		off = dataStart + chunkSize + (chunkSize % 2)
	}
	return 1
}

func exifOrientation(seg []byte) (int, bool) {
	if len(seg) < 14 || string(seg[0:6]) != "Exif\x00\x00" {
		return 1, false
	}
	return tiffOrientation(seg[6:])
}

// tiffOrientation reads the Orientation tag (0x0112) from a TIFF header.
func tiffOrientation(tiff []byte) (int, bool) {
	if len(tiff) < 8 {
		return 1, false
	}
	var bo binary.ByteOrder
	switch {
	case string(tiff[0:2]) == "II":
		bo = binary.LittleEndian
	case string(tiff[0:2]) == "MM":
		bo = binary.BigEndian
	default:
		return 1, false
	}
	ifdOff := int(bo.Uint32(tiff[4:8]))
	if ifdOff+2 > len(tiff) {
		return 1, false
	}
	count := int(bo.Uint16(tiff[ifdOff : ifdOff+2]))
	p := ifdOff + 2
	for n := 0; n < count && p+12 <= len(tiff); n++ {
		tag := bo.Uint16(tiff[p : p+2])
		if tag == 0x0112 { // Orientation
			val := int(bo.Uint16(tiff[p+8 : p+10]))
			if val >= 1 && val <= 8 {
				return val, true
			}
			return 1, false
		}
		p += 12
	}
	return 1, false
}

// applyOrientation rotates/flips img per the EXIF orientation value (1-8),
// matching pi's applyExifOrientation pixel mapping.
func applyOrientation(img image.Image, orientation int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	transform := func(dstW, dstH int, mapXY func(x, y int) (int, int)) image.Image {
		dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
		for y := 0; y < dstH; y++ {
			for x := 0; x < dstW; x++ {
				sx, sy := mapXY(x, y)
				dst.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
			}
		}
		return dst
	}
	switch orientation {
	case 2: // flip horizontal
		return transform(w, h, func(x, y int) (int, int) { return w - 1 - x, y })
	case 3: // rotate 180
		return transform(w, h, func(x, y int) (int, int) { return w - 1 - x, h - 1 - y })
	case 4: // flip vertical
		return transform(w, h, func(x, y int) (int, int) { return x, h - 1 - y })
	case 5: // transpose
		return transform(h, w, func(x, y int) (int, int) { return y, x })
	case 6: // rotate 90 CW
		return transform(h, w, func(x, y int) (int, int) { return y, h - 1 - x })
	case 7: // transverse
		return transform(h, w, func(x, y int) (int, int) { return w - 1 - y, h - 1 - x })
	case 8: // rotate 90 CCW
		return transform(h, w, func(x, y int) (int, int) { return w - 1 - y, x })
	default:
		return img
	}
}
