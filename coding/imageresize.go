package coding

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"math/big"
	"strconv"
	"strings"

	_ "golang.org/x/image/bmp"  // register BMP decoder for BMP→PNG conversion
	_ "golang.org/x/image/webp" // decode-only, to match photon's webp support

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// Image post-processing for the read tool (port of pi's resizeImageInProcess in
// utils/image-resize-core.ts). Downscales images that exceed the inline limits
// before sending them to the model and applies EXIF orientation. The decision
// surface (target dimensions, format choice, wasResized) is a faithful port and
// is differentially tested against pi (see imageresize_parity_test.go). The
// pixel data itself is not byte-identical: pi uses Photon/Lanczos3 and Rust
// encoders, this uses a pure-Go bilinear resize and the std-lib encoders.

const (
	imgMaxWidth           = 2000
	imgMaxHeight          = 2000
	imgMaxBase64Bytes     = int(4.5 * 1024 * 1024) // 4.5MB base64, headroom below Anthropic's 5MB
	imgDefaultJPEGQuality = 80
)

// resizeProfile is pi's `{ ...DEFAULT_OPTIONS, ...options }` — the resize
// limits after a model's ai.ModelImageResizeOptions has narrowed the defaults.
type resizeProfile struct {
	maxWidth      int
	maxHeight     int
	maxBytes      int
	jpegQualities []int
}

// resolveResizeProfile applies a model's resize options over the pipeline
// defaults. A nil profile, or a nil field inside one, keeps the default: the
// catalog stamps the same 2000/2000/4.5MiB/80 numbers onto every vision model,
// and a provider narrows individual limits from there (pi f5c946480).
func resolveResizeProfile(o *ai.ModelImageResizeOptions) resizeProfile {
	p := resizeProfile{maxWidth: imgMaxWidth, maxHeight: imgMaxHeight, maxBytes: imgMaxBase64Bytes}
	quality := imgDefaultJPEGQuality
	if o != nil {
		if o.MaxWidth != nil {
			p.maxWidth = *o.MaxWidth
		}
		if o.MaxHeight != nil {
			p.maxHeight = *o.MaxHeight
		}
		if o.MaxBytes != nil {
			p.maxBytes = *o.MaxBytes
		}
		if o.JPEGQuality != nil {
			quality = *o.JPEGQuality
		}
	}
	// pi: Array.from(new Set([opts.jpegQuality, 85, 70, 55, 40])) — insertion
	// order, first occurrence wins, so a configured 70 moves to the front and
	// drops out of the tail.
	for _, q := range []int{quality, 85, 70, 55, 40} {
		seen := false
		for _, have := range p.jpegQualities {
			if have == q {
				seen = true
				break
			}
		}
		if !seen {
			p.jpegQualities = append(p.jpegQualities, q)
		}
	}
	return p
}

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
func resizeImage(inputBytes []byte, mimeType string, opts *ai.ModelImageResizeOptions) (ResizeResult, bool) {
	p := resolveResizeProfile(opts)
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
	if ow <= p.maxWidth && oh <= p.maxHeight && inputB64 < p.maxBytes {
		return ResizeResult{
			Data: inputBytes, MimeType: mimeType,
			OriginalWidth: ow, OriginalHeight: oh,
			Width: ow, Height: oh, WasResized: false,
		}, true
	}

	// Initial target: scale to fit within max dimensions, preserving aspect.
	// pi uses Math.round for the dependent dimension.
	tw, th := ow, oh
	if tw > p.maxWidth {
		th = jsRound(float64(th) * float64(p.maxWidth) / float64(tw))
		tw = p.maxWidth
	}
	if th > p.maxHeight {
		tw = jsRound(float64(tw) * float64(p.maxHeight) / float64(th))
		th = p.maxHeight
	}

	// Shrink-and-encode loop: at each size try the smaller of PNG and the first
	// JPEG quality, then the other, then the lower JPEG qualities, taking the
	// first candidate under the byte limit; otherwise scale down by
	// 0.75 (floored) until 1×1 (mirrors pi's while loop exactly).
	cw, ch := tw, th
	for {
		scaled := oriented
		if cw != ow || ch != oh {
			scaled = bilinearResize(oriented, cw, ch)
		}
		if enc, mime, fit := encodeUnderLimit(scaled, p); fit {
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
func ResizeImageDecision(data []byte, mimeType string, resize *ai.ModelImageResizeOptions) (ResizeResult, bool) {
	return resizeImage(data, mimeType, resize)
}

// resizeImageForModel is a thin wrapper retained for the read tool: it returns
// the bytes + mime to embed, ok=false when the image can't be brought under the
// limit. (The richer decision is available via resizeImage.)
func resizeImageForModel(data []byte, mimeType string, resize *ai.ModelImageResizeOptions) (out []byte, outMime string, ok bool) {
	r, ok := resizeImage(data, mimeType, resize)
	if !ok {
		return nil, "", false
	}
	return r.Data, r.MimeType, true
}

// ProcessImageResult mirrors pi's discriminated ProcessImageResult
// (utils/image-process.ts). On success Ok is true and Data/MimeType/Hints are
// populated; on failure Ok is false and Message holds the omission note.
type ProcessImageResult struct {
	Ok       bool
	Data     []byte // raw image bytes to attach (not base64)
	MimeType string
	Hints    []string

	Message string
}

// supportedInlineImageMimeTypes maps the mime types models accept inline. BMP
// (and any other non-listed type) must be converted to PNG before sending.
// Mirrors pi's normalizeSupportedImageMimeType.
func normalizeSupportedImageMimeType(mimeType string) string {
	switch baseMimeType(mimeType) {
	case "image/png":
		return "image/png"
	case "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "image/gif":
		return "image/gif"
	case "image/webp":
		return "image/webp"
	default:
		return ""
	}
}

// baseMimeType is pi's baseMimeType: the type before any parameters, trimmed
// as JavaScript trims and lower-cased.
func baseMimeType(mimeType string) string {
	base := mimeType
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = base[:i]
	}
	return strings.ToLower(jstext.Trim(base))
}

// convertImageBytesToPng decodes arbitrary image bytes and re-encodes as PNG,
// mirroring pi's convertImageBytesToPng (photon). Returns nil on decode failure
// (pi returns null). Only BMP reaches this path in the Go port today.
func convertImageBytesToPng(data []byte) []byte {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// conversionHint mirrors pi's conversionHint: emitted when the image was
// converted from one mime type to another.
func conversionHint(from, to string) string {
	if from == "" || from == to {
		return ""
	}
	return fmt.Sprintf("[Image converted from %s to %s.]", from, to)
}

// processImage is a faithful port of pi's processImage (utils/image-process.ts).
// It normalizes the image to a supported inline mime type (converting BMP→PNG),
// optionally auto-resizes it below the inline limit, and reports processing
// hints. The result mirrors pi's discriminated { ok } shape.
func processImage(data []byte, mimeType string, autoResizeImages bool, resize *ai.ModelImageResizeOptions) ProcessImageResult {
	normalizedMime := normalizeSupportedImageMimeType(mimeType)
	normBytes := data
	convertedFrom := ""
	if normalizedMime == "" {
		pngBytes := convertImageBytesToPng(data)
		if pngBytes == nil {
			return ProcessImageResult{
				Ok:      false,
				Message: "[Image omitted: could not be converted to a supported inline image format.]",
			}
		}
		normBytes = pngBytes
		normalizedMime = "image/png"
		convertedFrom = baseMimeType(mimeType)
	}

	if autoResizeImages {
		resized, ok := resizeImage(normBytes, normalizedMime, resize)
		if !ok {
			return ProcessImageResult{
				Ok:      false,
				Message: "[Image omitted: could not be resized below the inline image size limit.]",
			}
		}
		var hints []string
		if h := conversionHint(convertedFrom, resized.MimeType); h != "" {
			hints = append(hints, h)
		}
		if dn := formatDimensionNote(resized); dn != "" {
			hints = append(hints, dn)
		}
		return ProcessImageResult{Ok: true, Data: resized.Data, MimeType: resized.MimeType, Hints: hints}
	}

	var hints []string
	if h := conversionHint(convertedFrom, normalizedMime); h != "" {
		hints = append(hints, h)
	}
	return ProcessImageResult{Ok: true, Data: normBytes, MimeType: normalizedMime, Hints: hints}
}

// normalizeToolResultImages normalizes the image blocks of a tool result,
// porting pi's normalizeToolResultImages (utils/tool-result-images.ts).
//
// The `read` tool runs its images through processImage, but tools that produce
// images themselves (custom SDK tools, MCP bridges, screenshot tools) hand back
// arbitrary base64 payloads that go straight into session history and every
// subsequent provider request. Oversized images make the provider reject the
// whole conversation, not just the offending turn, so normalize them once as
// they enter history.
//
// The second return value reports whether anything changed, so callers can skip
// rewriting the result (pi returns the original array by identity).
// decodeNodeBase64 decodes base64 the way Node's Buffer.from(value, "base64")
// does, which is the semantics pi's image path relies on. Node ignores
// characters outside the base64 alphabet (whitespace, newlines, stray bytes),
// accepts the base64url alphabet alongside the standard one, treats padding as
// optional, and drops a trailing orphan character rather than failing.
//
// Go's base64.StdEncoding is strict about all three, so a whitespace-wrapped or
// base64url payload failed to decode and the image was passed through
// unresized — partially defeating the resize of tool-returned images.
//
// Node also STOPS at the first `=`: whatever follows it is ignored, alphabet
// characters included. Filtering `=` out like any other stray byte instead
// decoded base64 assembled from separately padded chunks ("QUJD=QUJD") as the
// concatenation of every chunk, and realigned the bit groups across the gap
// into bytes that appear in none of them. Measured against Node, row by row,
// in TestDecodeNodeBase64StopsAtPadding.
//
// It cannot fail, so it returns no error. The filter keeps only alphabet
// characters and stops before any `=`, the trim removes a lone remainder, and
// non-strict raw decoding accepts every length and trailing-bit pattern left
// after that. Node's Buffer.from never throws either. The impossible error is
// discarded HERE, beside the filter and trim that make it impossible, so an
// edit to either one is made next to the proof that depends on it.
func decodeNodeBase64(value string) []byte {
	var b strings.Builder
	b.Grow(len(value))
scan:
	for _, r := range value {
		switch {
		case r == '=':
			break scan
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/':
			b.WriteRune(r)
		case r == '-': // base64url
			b.WriteByte('+')
		case r == '_': // base64url
			b.WriteByte('/')
		}
	}
	cleaned := b.String()
	// A group of one leftover character carries no whole byte; Node discards it.
	if len(cleaned)%4 == 1 {
		cleaned = cleaned[:len(cleaned)-1]
	}
	out, _ := base64.RawStdEncoding.DecodeString(cleaned)
	return out
}

func normalizeToolResultImages(content ai.ContentList, resize *ai.ModelImageResizeOptions) (ai.ContentList, bool) {
	hasImage := false
	for _, block := range content {
		if _, ok := block.(ai.ImageContent); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return content, false
	}

	normalized := make(ai.ContentList, 0, len(content))
	changed := false
	for _, block := range content {
		img, ok := block.(ai.ImageContent)
		if !ok {
			normalized = append(normalized, block)
			continue
		}
		// autoResize matches the `read` tool's call: the Go port has no settings
		// manager, so images.autoResize is always on.
		processed := processImage(decodeNodeBase64(img.Data), img.MimeType, true, resize)
		// Unlike `read`, keep the original block whenever processing fails. The
		// tool already produced this image and the failure may just be an
		// unsupported payload — base64 that decodes to something that is not an
		// image lands here too — so passing it through preserves the behavior
		// tools have today instead of silently deleting their output.
		if !processed.Ok {
			normalized = append(normalized, block)
			continue
		}
		data := encodeBase64(processed.Data)
		if data == img.Data && processed.MimeType == img.MimeType && len(processed.Hints) == 0 {
			normalized = append(normalized, block)
			continue
		}
		normalized = append(normalized, ai.ImageContent{Data: data, MimeType: processed.MimeType})
		if len(processed.Hints) > 0 {
			normalized = append(normalized, ai.TextContent{Text: strings.Join(processed.Hints, "\n")})
		}
		changed = true
	}
	if !changed {
		return content, false
	}
	return normalized, true
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

// toFixed2 formats x with exactly two decimals the way JS Number.toFixed(2)
// does. The spec takes the integer n nearest x·100, computed EXACTLY on x's
// binary value, and of two equally near picks the LARGER — round half up —
// where strconv.FormatFloat rounds an exact tie to even, so the odd eighths
// differed (1.125: JS "1.13", FormatFloat "1.12"). Rational arithmetic keeps
// the non-ties exact as well: 1.005 is 1.00499… in binary and stays "1.00".
//
// Callers pass a scale ratio, positive and finite by construction; that is the
// only range this handles. A value SetFloat64 cannot represent falls back to
// FormatFloat rather than panicking.
func toFixed2(x float64) string {
	r := new(big.Rat).SetFloat64(x)
	if r == nil || x < 0 {
		return strconv.FormatFloat(x, 'f', 2, 64)
	}
	r.Mul(r, big.NewRat(100, 1))
	r.Add(r, big.NewRat(1, 2))
	digits := new(big.Int).Quo(r.Num(), r.Denom()).String() // floor: r is non-negative
	for len(digits) < 3 {
		digits = "0" + digits
	}
	return digits[:len(digits)-2] + "." + digits[len(digits)-2:]
}

func encodeUnderLimit(img image.Image, p resizeProfile) ([]byte, string, bool) {
	type candidate struct {
		data []byte
		mime string
	}
	encodeJPEG := func(q int) (candidate, bool) {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return candidate{}, false
		}
		return candidate{buf.Bytes(), "image/jpeg"}, true
	}
	var candidates []candidate
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err == nil {
		candidates = append(candidates, candidate{buf.Bytes(), "image/png"})
	}
	// pi prefers the smaller of PNG and the JPEG at the first quality step
	// by base64 length (PNG on a tie), and only then the lower JPEG qualities
	// (upstream e7a0df27c).
	lower := p.jpegQualities
	if len(lower) > 0 {
		lower = lower[1:]
		if jpg, ok := encodeJPEG(p.jpegQualities[0]); ok {
			if len(candidates) == 1 && base64Size(len(jpg.data)) < base64Size(len(candidates[0].data)) {
				candidates = []candidate{jpg, candidates[0]}
			} else {
				candidates = append(candidates, jpg)
			}
		}
	}
	for _, c := range candidates {
		if base64Size(len(c.data)) < p.maxBytes {
			return c.data, c.mime, true
		}
	}
	for _, q := range lower {
		if c, ok := encodeJPEG(q); ok && base64Size(len(c.data)) < p.maxBytes {
			return c.data, c.mime, true
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

// jpegOrientation extracts the EXIF orientation (1-8) from a JPEG, or 1 if
// absent.
func jpegOrientation(data []byte) int {
	off := findJpegTiffOffset(data)
	if off < 0 {
		return 1
	}
	return tiffOrientation(data, off)
}

// findJpegTiffOffset walks the JPEG marker segments and returns the offset of
// the TIFF header carried by the first APP1 with an "Exif\0\0" header, or -1.
// It mirrors pi's findJpegTiffOffset, leniency included:
//
//   - fill bytes (extra 0xFF ahead of a marker) are skipped one at a time;
//   - APP1 segments that are not EXIF — XMP, most commonly — are scanned past
//     rather than ending the scan;
//   - the Exif header is tested BEFORE the segment length is read, so an APP1
//     whose declared length overruns the file still decides the answer;
//   - the first Exif APP1 decides outright, even when its TIFF block carries no
//     Orientation tag;
//   - the walk does not stop at SOS/EOI: it keeps reading whatever bytes sit in
//     the entropy-coded scan data as a marker and a length, which terminates the
//     scan on the first non-0xFF byte it lands on.
func findJpegTiffOffset(data []byte) int {
	for off := 2; off < len(data)-1; {
		if data[off] != 0xFF {
			return -1
		}
		marker := data[off+1]
		if marker == 0xFF { // fill byte: a marker may be preceded by any number of them
			off++
			continue
		}
		if marker == 0xE1 { // APP1
			segStart := off + 4
			if segStart+6 > len(data) {
				return -1
			}
			if hasExifHeader(data[segStart:]) {
				return segStart + 6
			}
		}
		if off+4 > len(data) {
			return -1
		}
		off += 2 + int(binary.BigEndian.Uint16(data[off+2:off+4]))
	}
	return -1
}

var exifHeader = []byte("Exif\x00\x00")

// hasExifHeader reports whether b opens with the "Exif\0\0" APP1 header.
func hasExifHeader(b []byte) bool { return bytes.HasPrefix(b, exifHeader) }

// webpOrientation reads orientation from a WebP EXIF chunk (mirrors pi's
// findWebpTiffOffset + readOrientationFromTiff).
func webpOrientation(data []byte) int {
	off := findWebpTiffOffset(data)
	if off < 0 {
		return 1
	}
	return tiffOrientation(data, off)
}

// findWebpTiffOffset returns the offset of the TIFF header in the WebP EXIF
// chunk, or -1. Mirrors pi's findWebpTiffOffset, whose chunk size is assembled
// with JS bitwise operators and is therefore a SIGNED 32-bit value: a size with
// the top bit set is negative, so an EXIF chunk declaring one passes the
// end-of-file check and is read as a bare TIFF block (it is < 6). The one place
// this port parts from pi is a negative size on a chunk it has to walk past:
// pi moves the offset backwards there and can spin forever, so the walk stops
// instead (see docs/UPSTREAM.md, Divergences).
func findWebpTiffOffset(data []byte) int {
	for off := 12; off+8 <= len(data); {
		chunkID := string(data[off : off+4])
		chunkSize := int(int32(binary.LittleEndian.Uint32(data[off+4 : off+8])))
		dataStart := off + 8
		if chunkID == "EXIF" {
			if dataStart+chunkSize > len(data) {
				return -1
			}
			// Some WebP files prefix the TIFF header with "Exif\0\0".
			if chunkSize >= 6 && hasExifHeader(data[dataStart:]) {
				return dataStart + 6
			}
			return dataStart
		}
		if chunkSize < 0 {
			return -1
		}
		off = dataStart + chunkSize + chunkSize%2 // RIFF chunks are padded to even size
	}
	return -1
}

// tiffOrientation reads the Orientation tag (0x0112) out of the TIFF header at
// tiffStart, returning 1 when there is none — pi's readOrientationFromTiff makes
// no distinction between "no orientation" and "malformed", and neither does
// this. Its leniency is deliberate and load-bearing for parity: only "II"
// selects little-endian (every other byte-order field, well-formed or not, is
// read big-endian), the 0x2A magic is not checked, and the IFD walk is bounded
// by the whole buffer rather than by the enclosing JPEG segment or WebP chunk,
// so an IFD that lies outside its own segment is still read.
func tiffOrientation(data []byte, tiffStart int) int {
	if tiffStart+8 > len(data) {
		return 1
	}
	le := data[tiffStart] == 'I' && data[tiffStart+1] == 'I'
	// JS reads an out-of-range index as undefined, which its bitwise operators
	// coerce to 0; byteAt reproduces that, and keeps every read below in bounds
	// for the offsets pi is willing to compute (they can even be negative — see
	// read32).
	byteAt := func(pos int) int {
		if pos < 0 || pos >= len(data) {
			return 0
		}
		return int(data[pos])
	}
	read16 := func(pos int) int {
		if le {
			return byteAt(pos) | byteAt(pos+1)<<8
		}
		return byteAt(pos)<<8 | byteAt(pos+1)
	}
	// pi's read32 is JS-signed in the little-endian branch (`b3 << 24` overflows
	// into the sign bit) and explicitly unsigned in the big-endian one.
	read32 := func(pos int) int {
		if le {
			u := uint32(byteAt(pos+3))<<24 | uint32(byteAt(pos+2))<<16 | uint32(byteAt(pos+1))<<8 | uint32(byteAt(pos))
			return int(int32(u))
		}
		return int(uint32(byteAt(pos))<<24 | uint32(byteAt(pos+1))<<16 | uint32(byteAt(pos+2))<<8 | uint32(byteAt(pos+3)))
	}

	ifdStart := tiffStart + read32(tiffStart+4)
	if ifdStart+2 > len(data) {
		return 1
	}
	for n, count := 0, read16(ifdStart); n < count; n++ {
		entry := ifdStart + 2 + n*12
		if entry+12 > len(data) {
			return 1
		}
		if read16(entry) == 0x0112 { // Orientation
			if v := read16(entry + 8); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
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
