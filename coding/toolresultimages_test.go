package coding

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/image/bmp"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// grayPNGBase64 builds an 8-bit grayscale PNG of the given size, base64-encoded
// the way a tool would hand it back.
func grayPNGBase64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x + y) % 256)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return encodeBase64(buf.Bytes())
}

func pngDimensions(t *testing.T, data string) (int, int) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestNormalizeToolResultImagesNoImages(t *testing.T) {
	content := ai.ContentList{ai.TextContent{Text: "no images here"}}
	out, changed := normalizeToolResultImages(content, nil)
	if changed {
		t.Fatal("content without images must be reported unchanged")
	}
	if len(out) != 1 {
		t.Fatalf("content rewritten: %#v", out)
	}
}

func TestNormalizeToolResultImagesWithinLimits(t *testing.T) {
	small := grayPNGBase64(t, 8, 8)
	content := ai.ContentList{
		ai.TextContent{Text: "screenshot"},
		ai.ImageContent{Data: small, MimeType: "image/png"},
	}
	out, changed := normalizeToolResultImages(content, nil)
	if changed {
		t.Fatal("image within limits must be reported unchanged")
	}
	if img, _ := out[1].(ai.ImageContent); img.Data != small {
		t.Fatal("image data rewritten")
	}
}

func TestNormalizeToolResultImagesResizesOversized(t *testing.T) {
	content := ai.ContentList{ai.ImageContent{Data: grayPNGBase64(t, 2400, 4800), MimeType: "image/png"}}

	out, changed := normalizeToolResultImages(content, nil)
	if !changed {
		t.Fatal("oversized image must be normalized")
	}
	if len(out) != 2 {
		t.Fatalf("expected image + hint block, got %#v", out)
	}
	img, ok := out[0].(ai.ImageContent)
	if !ok {
		t.Fatalf("first block is not an image: %#v", out[0])
	}
	w, h := pngDimensions(t, img.Data)
	if w > imgMaxWidth || h > imgMaxHeight {
		t.Fatalf("image not downscaled: %dx%d", w, h)
	}
	note, ok := out[1].(ai.TextContent)
	if !ok {
		t.Fatalf("second block is not text: %#v", out[1])
	}
	if !strings.Contains(note.Text, "original 2400x4800") {
		t.Fatalf("hint missing original dimensions: %q", note.Text)
	}
}

func TestNormalizeToolResultImagesConvertsUnsupportedFormat(t *testing.T) {
	content := ai.ContentList{ai.ImageContent{Data: encodeBase64(tinyBMP1x1Red24bpp()), MimeType: "image/bmp"}}

	out, changed := normalizeToolResultImages(content, nil)
	if !changed {
		t.Fatal("unsupported format must be converted")
	}
	img, ok := out[0].(ai.ImageContent)
	if !ok || img.MimeType != "image/png" {
		t.Fatalf("expected PNG image block, got %#v", out[0])
	}
	note, ok := out[1].(ai.TextContent)
	if !ok || note.Text != "[Image converted from image/bmp to image/png.]" {
		t.Fatalf("unexpected conversion hint: %#v", out[1])
	}
}

func TestNormalizeToolResultImagesKeepsUndecodable(t *testing.T) {
	content := ai.ContentList{ai.ImageContent{Data: "bm90LWFuLWltYWdl", MimeType: "image/png"}}
	out, changed := normalizeToolResultImages(content, nil)
	if changed {
		t.Fatal("undecodable image must be kept as-is, not dropped")
	}
	if img, _ := out[0].(ai.ImageContent); img.Data != "bm90LWFuLWltYWdl" {
		t.Fatalf("undecodable image was rewritten: %#v", out[0])
	}
}

func TestNormalizeToolResultImagesPreservesSurroundingText(t *testing.T) {
	content := ai.ContentList{
		ai.TextContent{Text: "before"},
		ai.ImageContent{Data: grayPNGBase64(t, 2400, 100), MimeType: "image/png"},
		ai.TextContent{Text: "after"},
	}

	out, changed := normalizeToolResultImages(content, nil)
	if !changed {
		t.Fatal("oversized image must be normalized")
	}
	var kinds []string
	for _, block := range out {
		switch block.(type) {
		case ai.TextContent:
			kinds = append(kinds, "text")
		case ai.ImageContent:
			kinds = append(kinds, "image")
		default:
			kinds = append(kinds, "other")
		}
	}
	if strings.Join(kinds, ",") != "text,image,text,text" {
		t.Fatalf("unexpected block order: %v", kinds)
	}
	if first, _ := out[0].(ai.TextContent); first.Text != "before" {
		t.Fatalf("leading text changed: %#v", out[0])
	}
	if last, _ := out[3].(ai.TextContent); last.Text != "after" {
		t.Fatalf("trailing text changed: %#v", out[3])
	}
}

// screenshotTool stands in for custom SDK tools, MCP bridges, or screenshot
// tools that return images they produced themselves.
func screenshotTool(data string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "screenshot",
		Label:       "Screenshot",
		Description: "Return an oversized screenshot",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{
				ai.TextContent{Text: "captured"},
				ai.ImageContent{Data: data, MimeType: "image/png"},
			}, Details: map[string]any{}}, nil
		},
	}
}

func runScreenshotSession(t *testing.T, opts SessionOptions) []ai.ImageContent {
	t.Helper()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("screenshot", map[string]any{}, "c1"),
		}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "done"}}, ai.StopStop)),
	})
	opts.Model = reg.GetModel()
	opts.Cwd = t.TempDir()

	res, err := newTestSession(t, opts).Run(context.Background(), "take a screenshot")
	if err != nil {
		t.Fatal(err)
	}
	var images []ai.ImageContent
	for _, m := range res.Messages {
		tr, ok := m.(ai.ToolResultMessage)
		if !ok {
			continue
		}
		for _, block := range tr.Content {
			if img, ok := block.(ai.ImageContent); ok {
				images = append(images, img)
			}
		}
	}
	return images
}

// TestSessionResizesToolResultImages locks pi's fix: images returned by tools
// are resized before they enter session history.
func TestSessionResizesToolResultImages(t *testing.T) {
	oversized := grayPNGBase64(t, 2400, 4800)
	images := runScreenshotSession(t, SessionOptions{CustomTools: []agent.AgentTool{screenshotTool(oversized)}})

	if len(images) != 1 {
		t.Fatalf("expected one image in history, got %d", len(images))
	}
	w, h := pngDimensions(t, images[0].Data)
	if w > imgMaxWidth || h > imgMaxHeight {
		t.Fatalf("tool result image not resized: %dx%d", w, h)
	}
}

// TestSessionResizesImagesInjectedByAfterToolCall locks that normalization runs
// AFTER the hook, so images the hook injects are normalized too.
func TestSessionResizesImagesInjectedByAfterToolCall(t *testing.T) {
	oversized := grayPNGBase64(t, 2400, 4800)
	small := grayPNGBase64(t, 8, 8)
	images := runScreenshotSession(t, SessionOptions{
		CustomTools: []agent.AgentTool{screenshotTool(small)},
		AfterToolCall: func(ctx context.Context, c agent.AfterToolCallContext) *agent.AfterToolCallResult {
			return &agent.AfterToolCallResult{
				Content:    ai.ContentList{ai.ImageContent{Data: oversized, MimeType: "image/png"}},
				HasContent: true,
			}
		},
	})

	if len(images) != 1 {
		t.Fatalf("expected one image in history, got %d", len(images))
	}
	w, h := pngDimensions(t, images[0].Data)
	if w > imgMaxWidth || h > imgMaxHeight {
		t.Fatalf("hook-injected image not resized: %dx%d", w, h)
	}
}

// TestNormalizeToolResultImagesLenientBase64 locks the decode of tool-returned
// image payloads to Node's Buffer.from(x, "base64"). Go's StdEncoding happens to
// skip \r and \n, but it rejects the base64url alphabet and spaces outright, so
// such a payload failed to decode and the oversized image was passed through
// UNRESIZED, partially defeating the resize of tool-returned images.
func TestNormalizeToolResultImagesLenientBase64(t *testing.T) {
	oversized := grayPNGBase64(t, 3000, 100)

	// base64url: the alphabet a tool emitting URL-safe base64 would produce.
	urlSafe := strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(oversized)
	// Space-separated groups: whitespace Node ignores and Go rejects.
	var spaced strings.Builder
	for i := 0; i < len(oversized); i += 76 {
		end := min(i+76, len(oversized))
		spaced.WriteString(oversized[i:end])
		spaced.WriteByte(' ')
	}

	for name, payload := range map[string]string{"base64url": urlSafe, "spaces": spaced.String()} {
		t.Run(name, func(t *testing.T) {
			content := ai.ContentList{ai.ImageContent{Data: payload, MimeType: "image/png"}}
			out, changed := normalizeToolResultImages(content, nil)
			if !changed {
				t.Fatal("oversized image was not resized; the decode is stricter than Node's")
			}
			img, ok := out[0].(ai.ImageContent)
			if !ok {
				t.Fatalf("expected an image block, got %T", out[0])
			}
			if w, _ := pngDimensions(t, img.Data); w > imgMaxWidth {
				t.Fatalf("resized width = %d, want <= %d", w, imgMaxWidth)
			}
		})
	}
}

// TestSessionNormalizesPromptImages pins upstream f5c946480's
// _normalizePromptImages: an image handed to Run is converted and resized
// against the session model's profile BEFORE it is recorded, and the pipeline's
// notes ride back on the user text. Previously Run put the caller's bytes into
// the transcript verbatim, so an oversized or non-inline image went to the
// provider untouched and its notes were lost.
func TestSessionNormalizesPromptImages(t *testing.T) {
	maxWidth, maxHeight := 40, 40
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{
			ID:    "faux-vision",
			Input: []string{"text", "image"},
			InputLimits: &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{
				Resize: &ai.ModelImageResizeOptions{MaxWidth: &maxWidth, MaxHeight: &maxHeight},
			}},
		}},
	})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)),
	})

	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})

	// A 300x240 BMP: outside the model's 40px profile AND not an inline type, so
	// the pipeline both converts and downscales it, and says so.
	img := image.NewRGBA(image.Rect(0, 0, 300, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 300; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 9, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Run(context.Background(), "look",
		ai.ImageContent{Data: encodeBase64(buf.Bytes()), MimeType: "image/bmp"}); err != nil {
		t.Fatal(err)
	}

	var user ai.UserMessage
	found := false
	for _, m := range sess.History() {
		if u, ok := m.(ai.UserMessage); ok {
			user, found = u, true
			break
		}
	}
	if !found {
		t.Fatalf("no user message recorded: %+v", sess.History())
	}
	var gotText string
	var gotImage *ai.ImageContent
	for _, c := range user.Content {
		switch v := c.(type) {
		case ai.TextContent:
			gotText = v.Text
		case ai.ImageContent:
			gotImage = &v
		}
	}
	if gotImage == nil {
		t.Fatalf("no image block recorded: %+v", user.Content)
	}
	if gotImage.MimeType == "image/bmp" {
		t.Fatal("BMP must be converted to an inline type before it is recorded")
	}
	dec, _, err := image.Decode(bytes.NewReader(decodeNodeBase64(gotImage.Data)))
	if err != nil {
		t.Fatal(err)
	}
	if b := dec.Bounds(); b.Dx() > maxWidth || b.Dy() > maxHeight {
		t.Fatalf("recorded image %dx%d exceeds the model profile %dx%d", b.Dx(), b.Dy(), maxWidth, maxHeight)
	}
	if !strings.HasPrefix(gotText, "look\n\n") {
		t.Fatalf("pipeline notes must follow the prompt text, got %q", gotText)
	}
	if !strings.Contains(gotText, "[Image converted from image/bmp to image/png.]") {
		t.Fatalf("expected the conversion note, got %q", gotText)
	}
	if !strings.Contains(gotText, "original 300x240, displayed at 40x32") {
		t.Fatalf("expected the dimension note, got %q", gotText)
	}
}

// TestSessionResizesToolResultImagesToModelProfile pins the second of
// f5c946480's two session-side sites (agent-session.ts:547): a tool result's
// images are resized against the CURRENT model's profile, not just the
// pipeline defaults. Without the profile reaching normalizeToolResultImages an
// image well inside the 2000px default sails through a model that asks for 40.
func TestSessionResizesToolResultImagesToModelProfile(t *testing.T) {
	maxWidth, maxHeight := 40, 40
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{
			ID:    "faux-vision",
			Input: []string{"text", "image"},
			InputLimits: &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{
				Resize: &ai.ModelImageResizeOptions{MaxWidth: &maxWidth, MaxHeight: &maxHeight},
			}},
		}},
	})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("screenshot", map[string]any{}, "c1"),
		}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "done"}}, ai.StopStop)),
	})

	// 300x240 is comfortably inside the 2000px default and outside the model's.
	sess := newTestSession(t, SessionOptions{
		Model:       reg.GetModel(),
		Cwd:         t.TempDir(),
		CustomTools: []agent.AgentTool{screenshotTool(grayPNGBase64(t, 300, 240))},
	})
	res, err := sess.Run(context.Background(), "take a screenshot")
	if err != nil {
		t.Fatal(err)
	}
	var images []ai.ImageContent
	for _, m := range res.Messages {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			for _, block := range tr.Content {
				if img, ok := block.(ai.ImageContent); ok {
					images = append(images, img)
				}
			}
		}
	}
	if len(images) != 1 {
		t.Fatalf("expected one image in history, got %d", len(images))
	}
	if w, h := pngDimensions(t, images[0].Data); w > maxWidth || h > maxHeight {
		t.Fatalf("tool result image %dx%d exceeds the model profile %dx%d", w, h, maxWidth, maxHeight)
	}
}

// TestReadToolUsesSessionModelProfile pins f5c946480's read.ts:117 site. pi
// reads the profile off the tool execution context's model; the port's
// AgentTool.Execute has no model, so NewSession installs a getter instead
// (toolModelFn). Reading it per call is what keeps a mid-session SetModel
// from leaving the tool on a stale profile.
func TestReadToolUsesSessionModelProfile(t *testing.T) {
	maxWidth, maxHeight := 40, 40
	narrow := &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{
		Resize: &ai.ModelImageResizeOptions{MaxWidth: &maxWidth, MaxHeight: &maxHeight},
	}}
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{
			{ID: "wide", Input: []string{"text", "image"}},
			{ID: "narrow", Input: []string{"text", "image"}, InputLimits: narrow},
		},
	})
	defer reg.Unregister()

	cwd := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 300, 240))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "shot.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	models := reg.Models
	wide, narrowModel := models[0], models[1]
	if wide.ID != "wide" {
		wide, narrowModel = models[1], models[0]
	}

	sess := newTestSession(t, SessionOptions{Model: wide, Cwd: cwd, ToolNames: []string{"read"}})
	readImage := func() (int, int) {
		t.Helper()
		var tool agent.AgentTool
		for _, candidate := range sess.Agent.State().Tools {
			if candidate.Name == "read" {
				tool = candidate
			}
		}
		res, err := tool.Execute(context.Background(), "1", map[string]any{"path": "shot.png"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range res.Content {
			if got, ok := block.(ai.ImageContent); ok {
				return pngDimensions(t, got.Data)
			}
		}
		t.Fatalf("read returned no image: %+v", res.Content)
		return 0, 0
	}

	// The wide model asks for nothing, so the defaults apply and 300x240 passes.
	if w, h := readImage(); w != 300 || h != 240 {
		t.Fatalf("default profile should pass the image through, got %dx%d", w, h)
	}

	// Switching the model mid-session must reach the already-built tool.
	sess.SetModel(narrowModel, "")
	if w, h := readImage(); w > maxWidth || h > maxHeight {
		t.Fatalf("read image %dx%d exceeds the model profile %dx%d", w, h, maxWidth, maxHeight)
	}
}

// TestDecodeNodeBase64StopsAtPadding pins decodeNodeBase64 to what Node's
// Buffer.from(value, "base64") actually returns. Every want below was captured
// by running Node, not reasoned: Node decodes the alphabet characters that come
// BEFORE the first `=` and ignores everything after it. The port used to filter
// `=` out like any other stray byte and keep going, so base64 built by
// concatenating separately padded chunks — "QUJD=QUJD" — decoded to both chunks
// in Go and to the first one only in pi. Found by this cycle's JS-semantics
// review; this cycle newly routes prompt images through the same decoder.
func TestDecodeNodeBase64StopsAtPadding(t *testing.T) {
	cases := []struct {
		in   string
		want []byte
	}{
		{"QUJD", []byte("ABC")},
		{"QUJD=QUJD", []byte("ABC")},
		{"QUJD==QUJD", []byte("ABC")},
		{"QUJD===", []byte("ABC")},
		{"QUJDQU=JD", []byte("ABCA")},
		{"QUJDQ=UJD", []byte("ABC")},
		{"QU=JD", []byte("A")},
		{"QUJ=D", []byte("AB")},
		{"QUI=QUJD", []byte("AB")},
		{"QQ==QUJD", []byte("A")},
		{"QU\n=JD", []byte("A")},
		{"=QUJD", []byte{}},
		{" =QUJD", []byte{}},
		{"Q=UJD", []byte{}},
		// Unchanged behaviour, kept beside the new rule so a regression in
		// either direction shows up here.
		{"QUJDA", []byte("ABC")},
		{"QUJD-_", []byte{65, 66, 67, 251}},
		{"Q U J D", []byte("ABC")},
		{"QU!JD", []byte("ABC")},
		{"!!!!", []byte{}},
	}
	for _, c := range cases {
		if got := decodeNodeBase64(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("decodeNodeBase64(%q) = %v, Node gives %v", c.in, got, c.want)
		}
	}
}

// TestSessionReportsUnprocessablePromptImages pins the failure half of
// _normalizePromptImages: an image the pipeline cannot handle contributes pi's
// omission note to the user text and no block, and the turn still goes out.
// The two notes are captured from the 0.86.1 build's processImage over the
// same bytes ("not an image"), one per branch — a supported inline type that
// will not decode, and a type that must be converted and cannot be.
func TestSessionReportsUnprocessablePromptImages(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-vision", Input: []string{"text", "image"}}},
	})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)),
	})
	sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})

	const garbage = "bm90IGFuIGltYWdl" // base64("not an image")
	if _, err := sess.Run(context.Background(), "look",
		ai.ImageContent{Data: garbage, MimeType: "image/png"},
		ai.ImageContent{Data: garbage, MimeType: "image/bmp"}); err != nil {
		t.Fatal(err)
	}
	var user ai.UserMessage
	for _, m := range sess.History() {
		if u, ok := m.(ai.UserMessage); ok {
			user = u
			break
		}
	}
	const want = "look\n\n" +
		"[Image omitted: could not be resized below the inline image size limit.]\n" +
		"[Image omitted: could not be converted to a supported inline image format.]"
	if len(user.Content) != 1 {
		t.Fatalf("an unprocessable image must contribute no block, got %d blocks: %+v", len(user.Content), user.Content)
	}
	if text, _ := user.Content[0].(ai.TextContent); text.Text != want {
		t.Fatalf("user text = %q, want pi's %q", text.Text, want)
	}
}

// TestImageResizeProfileIsRaceFreeAcrossSetModel pins the synchronization the
// resize getter needs. pi reads `this.agent.state.model` (agent-session.ts);
// the getter first shipped reading the plain Session.Model field, which
// SetModel writes without a lock, from the tool-execution goroutine. An SDK
// embedder switching models while a turn's read tool loads an image is exactly
// the case the getter's own comment advertises. Run under -race: the defect is
// a data race, which only the race detector can see. Filed independently by
// this cycle's go and parity reviews.
func TestImageResizeProfileIsRaceFreeAcrossSetModel(t *testing.T) {
	maxWidth := 40
	narrow := &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{
		Resize: &ai.ModelImageResizeOptions{MaxWidth: &maxWidth},
	}}
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{
			{ID: "wide", Input: []string{"text", "image"}},
			{ID: "narrow", Input: []string{"text", "image"}, InputLimits: narrow},
		},
	})
	defer reg.Unregister()

	cwd := t.TempDir()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "shot.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sess := newTestSession(t, SessionOptions{Model: reg.Models[0], Cwd: cwd, ToolNames: []string{"read"}})
	var read agent.AgentTool
	for _, tool := range sess.Agent.State().Tools {
		if tool.Name == "read" {
			read = tool
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			sess.SetModel(reg.Models[i%2], "")
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := read.Execute(context.Background(), "1", map[string]any{"path": "shot.png"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

// TestReadImageNoteFollowsTheModel replays testdata/readimagenote
// (pi-coding-agent 1.3.0's read tool): when the request's model does not take
// images, the text that goes with an image — processed or not — ends with
// getNonVisionImageNote's line, and the structured content's note carries it
// too. A vision model, or no model at all, adds nothing.
func TestReadImageNoteFollowsTheModel(t *testing.T) {
	data, err := os.ReadFile("testdata/readimagenote/readimagenote-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			File       string          `json:"file"`
			Model      string          `json:"model"`
			ModelInput []string        `json:"modelInput"`
			Text       string          `json:"text"`
			Blocks     []string        `json:"blocks"`
			Structured json.RawMessage `json:"structured"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) == 0 {
		t.Fatal("the readimagenote capture holds no rows")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pixel.png"), minimalPNG(), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.png"), minimalPNG()[:33], 0o644)
	for _, row := range capture.Rows {
		t.Run(row.File+"/"+row.Model, func(t *testing.T) {
			var model *ai.Model
			if row.ModelInput != nil {
				model = &ai.Model{ID: row.Model, Provider: "p", Api: "a", Input: row.ModelInput}
			}
			res, err := readToolOps(dir, nil, func() *ai.Model { return model }).Execute(context.Background(), "id", map[string]any{"path": row.File}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var blocks []string
			text := ""
			for _, block := range res.Content {
				switch b := block.(type) {
				case ai.TextContent:
					blocks = append(blocks, "text")
					text = b.Text
				case ai.ImageContent:
					blocks = append(blocks, "image")
				}
			}
			if text != row.Text || !slices.Equal(blocks, row.Blocks) {
				t.Fatalf("text %q blocks %q, pi %q %q", text, blocks, row.Text, row.Blocks)
			}
			structured := res.StructuredContent
			if image, ok := structured.(ReadToolImage); ok {
				image.Data = "string" // the capture records the data's type, not its bytes
				structured = image
			}
			got, err := json.Marshal(structured)
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := json.Compact(&want, row.Structured); err != nil {
				t.Fatal(err)
			}
			if string(got) != want.String() {
				t.Fatalf("structured content %s, pi %s", got, want.String())
			}
		})
	}
}
