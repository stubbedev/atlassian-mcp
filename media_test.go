package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kettek/apng"
)

// Fixtures in testdata/media: cuts.mp4 is H.264 High with B-frames — 2 s of
// testsrc2, 2 s of SMPTE bars, 2 s of a Mandelbrot zoom, so ffmpeg's scene
// filter finds cuts at exactly 2 s and 4 s. The still.* images come from the
// gen2brain decoders' own test data.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/media/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func frameTimes(r *processVideoResult) []string {
	out := make([]string, 0, len(r.frames))
	for _, f := range r.frames {
		out = append(out, fmt.Sprintf("%.2f", f.timestampSec))
	}
	return out
}

func decodeJPEG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("frame is not a JPEG: %v", err)
	}
	return img
}

func TestVideoUniform(t *testing.T) {
	res, err := processVideo(readFixture(t, "cuts.mp4"), processVideoOpts{frames: 6, mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.meta
	if m.codec != "h264" || m.width != 160 || m.height != 96 || math.Abs(m.duration-6) > 0.1 || math.Abs(m.fps-15) > 0.5 {
		t.Errorf("meta = %+v", m)
	}
	// Six slices of 6 s, each sampled at its middle frame.
	if got := strings.Join(frameTimes(res), " "); got != "0.47 1.47 2.47 3.47 4.47 5.47" {
		t.Errorf("times = %s", got)
	}
	img := decodeJPEG(t, res.frames[3].data)
	if b := img.Bounds(); b.Dx() != 160 || b.Dy() != 96 {
		t.Errorf("frame size = %v", b)
	}
	// 3.47 s is in the SMPTE bars; their leftmost bar is light grey, so a
	// color or range mistake in YUV→RGB shows up here.
	r, g, b, _ := img.At(10, 20).RGBA()
	if r>>8 < 160 || g>>8 < 160 || b>>8 < 160 || r>>8 > 215 {
		t.Errorf("bar color = %d,%d,%d", r>>8, g>>8, b>>8)
	}
}

func TestVideoScenesMatchFFmpeg(t *testing.T) {
	res, err := processVideo(readFixture(t, "cuts.mp4"), processVideoOpts{frames: 6, mode: "scenes", sceneThreshold: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if res.mode != "scenes" || strings.Join(frameTimes(res), " ") != "2.00 4.00" {
		t.Errorf("mode %s, times %v; ffmpeg's select finds 2.00 4.00", res.mode, frameTimes(res))
	}
}

func TestVideoWindow(t *testing.T) {
	start, end := 2.0, 4.0
	res, err := processVideo(readFixture(t, "cuts.mp4"), processVideoOpts{frames: 4, mode: "uniform", start: &start, end: &end})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.frames {
		if f.timestampSec < start || f.timestampSec > end {
			t.Errorf("frame at %.2f outside window", f.timestampSec)
		}
	}
	if _, err := processVideo(readFixture(t, "cuts.mp4"), processVideoOpts{frames: 4, start: &end, end: &start}); err == nil {
		t.Error("inverted window accepted")
	}
}

func TestVideoDedupDropsStaticFrames(t *testing.T) {
	// The SMPTE bars are still for 2 s: sampling densely there must collapse.
	start, end := 2.2, 3.8
	res, err := processVideo(readFixture(t, "cuts.mp4"), processVideoOpts{frames: 5, mode: "uniform", dedup: true, start: &start, end: &end})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.frames) != 1 {
		t.Errorf("kept %d frames of a still scene", len(res.frames))
	}
}

func TestVideoRejectsUnsupported(t *testing.T) {
	_, err := processVideo([]byte("definitely not a video"), processVideoOpts{frames: 3})
	if err == nil || !strings.Contains(err.Error(), "unsupported video format") {
		t.Errorf("err = %v", err)
	}
}

// animationColors are the solid colors of the generated test animations.
var animationColors = []color.NRGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 0, 255}}

func solid(c color.Color, w, h int) *image.Paletted {
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{c, color.Transparent})
	return img
}

func assertColorAt(t *testing.T, data []byte, want color.NRGBA) {
	t.Helper()
	r, g, b, _ := decodeJPEG(t, data).At(4, 4).RGBA()
	near := func(a uint32, b uint8) bool { return math.Abs(float64(a>>8)-float64(b)) < 40 }
	if !near(r, want.R) || !near(g, want.G) || !near(b, want.B) {
		t.Errorf("color %d,%d,%d, want %v", r>>8, g>>8, b>>8, want)
	}
}

func TestAnimatedGIF(t *testing.T) {
	g := &gif.GIF{}
	for _, c := range animationColors {
		g.Image = append(g.Image, solid(c, 16, 16))
		g.Delay = append(g.Delay, 50) // 0.5 s
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	if !isAnimatedImage(buf.Bytes(), "image/gif") {
		t.Fatal("not detected as animated")
	}
	res, err := processVideo(buf.Bytes(), processVideoOpts{frames: 4, mode: "uniform", dedup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.meta.codec != "gif" || res.meta.duration != 2 || len(res.frames) != 4 {
		t.Fatalf("meta %+v, %d frames", res.meta, len(res.frames))
	}
	for i, f := range res.frames {
		assertColorAt(t, f.data, animationColors[i])
	}
}

func TestAnimatedPNG(t *testing.T) {
	a := apng.APNG{}
	for i, c := range animationColors {
		img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
		for p := 0; p < len(img.Pix); p += 4 {
			img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = c.R, c.G, c.B, c.A
		}
		a.Frames = append(a.Frames, apng.Frame{Image: img, DelayNumerator: uint16(1 + i%2), DelayDenominator: 4})
	}
	var buf bytes.Buffer
	if err := apng.Encode(&buf, a); err != nil {
		t.Fatal(err)
	}
	res, err := processVideo(buf.Bytes(), processVideoOpts{frames: 4, mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	// Delays alternate 0.25 s and 0.5 s: 1.5 s in all.
	if res.meta.codec != "apng" || math.Abs(res.meta.duration-1.5) > 1e-9 {
		t.Fatalf("meta %+v", res.meta)
	}
	for _, f := range res.frames {
		i := 0
		for i+1 < len(animationColors) && f.timestampSec >= []float64{0, 0.25, 0.75, 1.0}[i+1] {
			i++
		}
		assertColorAt(t, f.data, animationColors[i])
	}
}

func TestAnimatedWebP(t *testing.T) {
	res, err := processVideo(readFixture(t, "anim.webp"), processVideoOpts{frames: 3, mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	if res.meta.codec != "webp" || res.meta.width != 96 || res.meta.height != 64 || len(res.frames) != 3 {
		t.Fatalf("meta %+v, %d frames", res.meta, len(res.frames))
	}
	decodeJPEG(t, res.frames[0].data)
}

func TestStillFormatsWithoutFFmpeg(t *testing.T) {
	for _, name := range []string{"still.avif", "still.heic", "still.jxl"} {
		data, mime, _, err := processImage(readFixture(t, name), "image/"+strings.TrimPrefix(name, "still."), 256, 80)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(data) == 0 || (mime != "image/jpeg" && mime != "image/png") {
			t.Errorf("%s: %s, %d bytes", name, mime, len(data))
		}
	}
}

// minimalPDF is one page holding a filled black rectangle and no text, so
// it takes the scanned-PDF path: rasterize instead of extracting text.
func minimalPDF() []byte {
	content := "0 0 0 rg 50 50 100 100 re f"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestRasterizePDFInProcess(t *testing.T) {
	imgs, err := rasterizePDF(minimalPDF(), 1, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("%d pages", len(imgs))
	}
	img := imgs[0]
	if b := img.Bounds(); b.Dx() != 400 || b.Dy() != 400 {
		t.Errorf("page size = %v", b)
	}
	// PDF y runs up: the rectangle spans 50–150 of 200, i.e. 100–300 px
	// either way, inside a white page.
	if r, _, _, _ := img.At(200, 200).RGBA(); r>>8 > 30 {
		t.Errorf("rectangle not drawn: %d", r>>8)
	}
	if r, _, _, _ := img.At(20, 20).RGBA(); r>>8 < 225 {
		t.Errorf("page not white: %d", r>>8)
	}

	res := buildPDFResult(attachmentArgs{id: "1", filename: "scan.pdf", buffer: minimalPDF()}, "scan.pdf")
	if len(res.Content) != 3 || res.Content[2].Type != "image" {
		t.Errorf("scanned PDF result: %+v", res.Content[0].Text)
	}
}

// The same 6 s clip in every container the demuxers handle. The MJPEG and
// VP8 fixtures are 80×48 at 5 and 15 fps; the VP8 one has a keyframe each
// second and an unknown-size segment, as live WebM writers produce.
func TestVideoContainers(t *testing.T) {
	for _, tc := range []struct {
		file, codec string
		w, h        int
	}{
		{"cuts.mkv", "h264", 160, 96},
		{"cuts.avi", "h264", 160, 96},
		{"mjpeg.avi", "mjpeg", 80, 48},
		{"mjpeg.mov", "mjpeg", 80, 48},
		{"vp8.webm", "vp8", 80, 48},
	} {
		t.Run(tc.file, func(t *testing.T) {
			res, err := processVideo(readFixture(t, tc.file), processVideoOpts{frames: 6, mode: "scenes", sceneThreshold: 0.3})
			if err != nil {
				t.Fatal(err)
			}
			m := res.meta
			if m.codec != tc.codec || m.width != tc.w || m.height != tc.h || math.Abs(m.duration-6) > 0.1 {
				t.Errorf("meta = %+v", m)
			}
			if res.mode != "scenes" || strings.Join(frameTimes(res), " ") != "2.00 4.00" {
				t.Errorf("mode %s, times %v; ffmpeg finds cuts at 2.00 4.00", res.mode, frameTimes(res))
			}
			decodeJPEG(t, res.frames[0].data)
		})
	}
}

func TestVP8SamplesKeyframes(t *testing.T) {
	res, err := processVideo(readFixture(t, "vp8.webm"), processVideoOpts{frames: 6, mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.frames {
		if f.timestampSec != math.Trunc(f.timestampSec) {
			t.Errorf("frame at %.2f is not a keyframe (one per second)", f.timestampSec)
		}
	}
	if !strings.Contains(res.note, "keyframes") {
		t.Errorf("note = %q", res.note)
	}
}

// MediaRecorder leaves cluster sizes unknown too; rewrite the fixture's so
// each cluster must be ended by the next one's start.
func TestMKVUnknownSizeClusters(t *testing.T) {
	data := slices.Clone(readFixture(t, "vp8.webm"))
	id := []byte{0x1F, 0x43, 0xB6, 0x75}
	patched := 0
	for i := bytes.Index(data, id); i >= 0; {
		_, n, _, err := ebmlVint(data[i+4:], false)
		if err != nil {
			t.Fatal(err)
		}
		data[i+4] = byte(0xff >> (n - 1))
		for k := 1; k < n; k++ {
			data[i+4+k] = 0xff
		}
		patched++
		next := bytes.Index(data[i+4:], id)
		if next < 0 {
			break
		}
		i += 4 + next
	}
	if patched < 2 {
		t.Fatalf("only %d clusters", patched)
	}
	s, err := openMKVStream(data)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := openMKVStream(readFixture(t, "vp8.webm"))
	if !slices.Equal(s.times(), want.times()) {
		t.Errorf("times %v, want %v", s.times(), want.times())
	}
}

func TestVideoUnsupportedCodecNamed(t *testing.T) {
	data := bytes.Replace(readFixture(t, "cuts.mkv"), []byte("V_MPEG4/ISO/AVC"), []byte("V_MPEGH/ISO/HEV"), 1)
	_, err := processVideo(data, processVideoOpts{frames: 3})
	if err == nil || !strings.Contains(err.Error(), "unsupported video codec V_MPEGH/ISO/HEV") {
		t.Errorf("err = %v", err)
	}
}

// Motion JPEG in AVI often leaves out the Huffman tables, relying on the
// standard ones.
func TestMJPEGWithoutHuffmanTables(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, 16, 16))
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	var stripped []byte
	for i := 2; i+4 <= len(full); {
		n := 2 + (int(full[i+2])<<8 | int(full[i+3]))
		if full[i+1] == 0xda {
			stripped = append(append(slices.Clone(full[:2]), stripped...), full[i:]...)
			break
		}
		if full[i+1] != 0xc4 {
			stripped = append(stripped, full[i:i+n]...)
		}
		i += n
	}
	if bytes.Contains(stripped, []byte{0xff, 0xc4}) {
		t.Fatal("DHT not stripped")
	}
	if _, err := jpeg.Decode(bytes.NewReader(stripped)); err == nil {
		t.Fatal("expected the stripped frame to be undecodable as is")
	}
	if _, err := (mjpegDecoder{}).decode(stripped); err != nil {
		t.Errorf("with default tables: %v", err)
	}
}

// Truncated or corrupted files must fail cleanly, never crash the server.
func TestVideoCorruptInputDoesNotPanic(t *testing.T) {
	for _, name := range []string{"cuts.mp4", "cuts.mkv", "cuts.avi", "mjpeg.avi", "vp8.webm", "anim.webp"} {
		data := readFixture(t, name)
		for _, cut := range []int{len(data) / 3, len(data) * 2 / 3} {
			_, _ = processVideo(data[:cut], processVideoOpts{frames: 4, mode: "scenes", sceneThreshold: 0.3})
		}
		mangled := slices.Clone(data)
		for i := len(mangled) / 4; i < len(mangled); i += 97 {
			mangled[i] ^= 0x5a
		}
		_, _ = processVideo(mangled, processVideoOpts{frames: 4, mode: "uniform"})
	}
}
