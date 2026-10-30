package coding

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Image-pipeline *decision* parity against real pi (Photon/Lanczos3).
//
// The .golden.json files in testdata/imgparity were captured by feeding the
// images generated below (deterministically) to pi's own resizeImageInProcess
// from @earendil-works/pi-coding-agent. Pixel bytes cannot match (pi uses
// Photon/Lanczos3 and Rust encoders; this uses a pure-Go bilinear resize and
// the std-lib encoders), so this test asserts the *decision surface* matches pi
// exactly: target dimensions, chosen format, wasResized, and post-orientation
// dims.
//
// The image generators below MUST stay byte-stable, since the golden decisions
// were captured from those exact bytes.
//
// photo_like.golden.json and oriented_big.golden.json are src captures at
// e7a0df27c (resizeImageInProcess from packages/coding-agent/src/utils over the
// 1.4.0 build's photon-node): that commit takes the smaller of PNG and JPEG, and
// the 1.4.0 build predates it, so re-verify both against the first build that
// ships it. Control: the same capture at e7a0df27c^1 answers PNG for both and
// reproduces the other seven goldens.

type imgDecision struct {
	OriginalWidth  int    `json:"originalWidth"`
	OriginalHeight int    `json:"originalHeight"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	MimeType       string `json:"mimeType"`
	WasResized     bool   `json:"wasResized"`
}

func gradientImg(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 100, A: 255})
		}
	}
	return img
}

func noiseImg(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := (x*2654435761 + y*40503 + x*y*97) & 0xFFFFFF
			img.Set(x, y, color.RGBA{R: uint8(v), G: uint8(v >> 8), B: uint8(v >> 16), A: 255})
		}
	}
	return img
}

// photoLikeImg is smooth gradients with light deterministic noise, so PNG
// compresses it poorly: the shape of pi's photoLikeJpeg (#11551, e7a0df27c),
// with an exact integer LCG.
func photoLikeImg(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = (seed*1103515245 + 12345) & 0x7fffffff
			noise := float64(seed % 6)
			img.Set(x, y, color.RGBA{
				R: uint8(float64(x)/float64(w)*200 + noise),
				G: uint8(float64(y)/float64(h)*200 + noise),
				B: uint8(128 + math.Sin(float64(x)/30)*60 + noise),
				A: 255,
			})
		}
	}
	return img
}

func pngEncode(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func jpegEncode(img image.Image, q int) []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: q})
	return b.Bytes()
}

func imgSample(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	switch name {
	case "small_gradient":
		return pngEncode(gradientImg(100, 80)), "image/png"
	case "wide_gradient":
		return pngEncode(gradientImg(2500, 100)), "image/png"
	case "tall_gradient":
		return pngEncode(gradientImg(100, 2500)), "image/png"
	case "big_gradient":
		return pngEncode(gradientImg(3000, 2400)), "image/png"
	case "odd_aspect":
		return pngEncode(gradientImg(2501, 1337)), "image/png"
	case "photo_noise":
		return jpegEncode(noiseImg(3000, 2000), 92), "image/jpeg"
	case "photo_like":
		return jpegEncode(photoLikeImg(2400, 1600), 85), "image/jpeg"
	case "oriented_small":
		return injectExifOrientation(jpegEncode(gradientImg(40, 20), 95), 6), "image/jpeg"
	case "oriented_big":
		return injectExifOrientation(jpegEncode(gradientImg(2400, 1200), 92), 6), "image/jpeg"
	}
	t.Fatalf("unknown sample %q", name)
	return nil, ""
}

func TestImageDecisionParityWithPi(t *testing.T) {
	goldens, err := filepath.Glob("testdata/imgparity/*.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(goldens) == 0 {
		t.Fatal("no image parity goldens found")
	}
	for _, goldenPath := range goldens {
		name := strings.TrimSuffix(filepath.Base(goldenPath), ".golden.json")
		t.Run(name, func(t *testing.T) {
			data, mime := imgSample(t, name)
			r, ok := resizeImage(data, mime, nil)

			goldenData, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(bytes.TrimSpace(goldenData)) == "null" {
				if ok {
					t.Fatalf("pi omitted the image but Go resized it to %dx%d", r.Width, r.Height)
				}
				return
			}
			if !ok {
				t.Fatal("Go omitted the image but pi produced a result")
			}

			var want imgDecision
			if err := json.Unmarshal(goldenData, &want); err != nil {
				t.Fatal(err)
			}
			got := imgDecision{r.OriginalWidth, r.OriginalHeight, r.Width, r.Height, r.MimeType, r.WasResized}
			if got != want {
				t.Errorf("decision diverges from pi:\n pi : %+v\n go : %+v", want, got)
			}
		})
	}
}
