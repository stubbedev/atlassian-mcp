package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
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

func frameTimes(r *VideoResult) []string {
	out := make([]string, 0, len(r.Frames))
	for _, f := range r.Frames {
		out = append(out, fmt.Sprintf("%.2f", f.TimestampSec))
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
	res, err := ProcessVideo(readFixture(t, "cuts.mp4"), ProcessVideoOpts{Frames: 6, Mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Meta
	if m.Codec != "h264" || m.Width != 160 || m.Height != 96 || math.Abs(m.Duration-6) > 0.1 || math.Abs(m.FPS-15) > 0.5 {
		t.Errorf("meta = %+v", m)
	}
	// Six slices of 6 s, each sampled at its middle frame.
	if got := strings.Join(frameTimes(res), " "); got != "0.47 1.47 2.47 3.47 4.47 5.47" {
		t.Errorf("times = %s", got)
	}
	img := decodeJPEG(t, res.Frames[3].Data)
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
	res, err := ProcessVideo(readFixture(t, "cuts.mp4"), ProcessVideoOpts{Frames: 6, Mode: "scenes", SceneThreshold: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "scenes" || strings.Join(frameTimes(res), " ") != "2.00 4.00" {
		t.Errorf("mode %s, times %v; ffmpeg's select finds 2.00 4.00", res.Mode, frameTimes(res))
	}
}

func TestVideoWindow(t *testing.T) {
	start, end := 2.0, 4.0
	res, err := ProcessVideo(readFixture(t, "cuts.mp4"), ProcessVideoOpts{Frames: 4, Mode: "uniform", Start: &start, End: &end})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Frames {
		if f.TimestampSec < start || f.TimestampSec > end {
			t.Errorf("frame at %.2f outside window", f.TimestampSec)
		}
	}
	if _, err := ProcessVideo(readFixture(t, "cuts.mp4"), ProcessVideoOpts{Frames: 4, Start: &end, End: &start}); err == nil {
		t.Error("inverted window accepted")
	}
}

func TestVideoDedupDropsStaticFrames(t *testing.T) {
	// The SMPTE bars are still for 2 s: sampling densely there must collapse.
	start, end := 2.2, 3.8
	res, err := ProcessVideo(readFixture(t, "cuts.mp4"), ProcessVideoOpts{Frames: 5, Mode: "uniform", Dedup: true, Start: &start, End: &end})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) != 1 {
		t.Errorf("kept %d frames of a still scene", len(res.Frames))
	}
}

func TestVideoRejectsUnsupported(t *testing.T) {
	_, err := ProcessVideo([]byte("definitely not a video"), ProcessVideoOpts{Frames: 3})
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
	if !IsAnimatedImage(buf.Bytes(), "image/gif") {
		t.Fatal("not detected as animated")
	}
	res, err := ProcessVideo(buf.Bytes(), ProcessVideoOpts{Frames: 4, Mode: "uniform", Dedup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Codec != "gif" || res.Meta.Duration != 2 || len(res.Frames) != 4 {
		t.Fatalf("meta %+v, %d frames", res.Meta, len(res.Frames))
	}
	for i, f := range res.Frames {
		assertColorAt(t, f.Data, animationColors[i])
	}
}

func solidFrame(c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for p := 0; p < len(img.Pix); p += 4 {
		img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = c.R, c.G, c.B, c.A
	}
	return img
}

// encodeAPNG builds an APNG from same-sized frames the way encoders do: each
// frame is a PNG whose IDAT data moves into fdAT chunks behind a frame
// control. A static default image, when given, precedes the first frame
// control and is not part of the animation.
func encodeAPNG(t *testing.T, defaultImage image.Image, frames []image.Image, delays [][2]uint16) []byte {
	t.Helper()
	idat := func(img image.Image) (ihdr, data []byte) {
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		chunks, err := pngChunks(b.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range chunks {
			switch c.kind {
			case "IHDR":
				ihdr = c.data
			case "IDAT":
				data = append(data, c.data...)
			}
		}
		return ihdr, data
	}
	ihdr, _ := idat(frames[0])
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	out.Write(encodePNGChunk("IHDR", ihdr))
	actl := make([]byte, 8)
	binary.BigEndian.PutUint32(actl, uint32(len(frames)))
	out.Write(encodePNGChunk("acTL", actl))
	if defaultImage != nil {
		_, data := idat(defaultImage)
		out.Write(encodePNGChunk("IDAT", data))
	}
	seq := uint32(0)
	for i, f := range frames {
		b := f.Bounds()
		ctl := make([]byte, 26)
		binary.BigEndian.PutUint32(ctl, seq)
		binary.BigEndian.PutUint32(ctl[4:], uint32(b.Dx()))
		binary.BigEndian.PutUint32(ctl[8:], uint32(b.Dy()))
		binary.BigEndian.PutUint16(ctl[20:], delays[i][0])
		binary.BigEndian.PutUint16(ctl[22:], delays[i][1])
		out.Write(encodePNGChunk("fcTL", ctl))
		seq++
		_, data := idat(f)
		if i == 0 && defaultImage == nil {
			out.Write(encodePNGChunk("IDAT", data))
			continue
		}
		fdat := binary.BigEndian.AppendUint32(nil, seq)
		out.Write(encodePNGChunk("fdAT", append(fdat, data...)))
		seq++
	}
	out.Write(encodePNGChunk("IEND", nil))
	return out.Bytes()
}

func TestAnimatedPNG(t *testing.T) {
	frames := make([]image.Image, 0, len(animationColors))
	delays := make([][2]uint16, 0, len(animationColors))
	for i, c := range animationColors {
		frames = append(frames, solidFrame(c))
		delays = append(delays, [2]uint16{uint16(1 + i%2), 4})
	}
	for _, defaultImage := range []image.Image{nil, solidFrame(color.NRGBA{R: 255, B: 255, A: 255})} {
		res, err := ProcessVideo(encodeAPNG(t, defaultImage, frames, delays), ProcessVideoOpts{Frames: 4, Mode: "uniform"})
		if err != nil {
			t.Fatal(err)
		}
		// Delays alternate 0.25 s and 0.5 s: 1.5 s in all.
		if res.Meta.Codec != "apng" || math.Abs(res.Meta.Duration-1.5) > 1e-9 || res.Meta.Width != 16 {
			t.Fatalf("meta %+v", res.Meta)
		}
		for _, f := range res.Frames {
			i := 0
			for i+1 < len(animationColors) && f.TimestampSec >= []float64{0, 0.25, 0.75, 1.0}[i+1] {
				i++
			}
			assertColorAt(t, f.Data, animationColors[i])
		}
	}
}

func TestAPNGRejectsCorruptChunks(t *testing.T) {
	good := encodeAPNG(t, nil, []image.Image{solidFrame(animationColors[0])}, [][2]uint16{{1, 10}})
	bad := append([]byte(nil), good...)
	bad[len(bad)-20] ^= 0xFF
	if _, _, err := apngFrames(bad); err == nil {
		t.Error("a chunk with a bad checksum must be an error")
	}
	if _, _, err := apngFrames(good[:40]); err == nil {
		t.Error("a truncated APNG must be an error")
	}
}

func TestAnimatedWebP(t *testing.T) {
	res, err := ProcessVideo(readFixture(t, "anim.webp"), ProcessVideoOpts{Frames: 3, Mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Codec != "webp" || res.Meta.Width != 96 || res.Meta.Height != 64 || len(res.Frames) != 3 {
		t.Fatalf("meta %+v, %d frames", res.Meta, len(res.Frames))
	}
	decodeJPEG(t, res.Frames[0].Data)
}

func TestStillFormatsWithoutFFmpeg(t *testing.T) {
	for _, name := range []string{"still.avif", "still.heic", "still.jxl"} {
		data, mime, _, err := ProcessImage(readFixture(t, name), "image/"+strings.TrimPrefix(name, "still."), 256, 80)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(data) == 0 || (mime != "image/jpeg" && mime != "image/png") {
			t.Errorf("%s: %s, %d bytes", name, mime, len(data))
		}
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
			res, err := ProcessVideo(readFixture(t, tc.file), ProcessVideoOpts{Frames: 6, Mode: "scenes", SceneThreshold: 0.3})
			if err != nil {
				t.Fatal(err)
			}
			m := res.Meta
			if m.Codec != tc.codec || m.Width != tc.w || m.Height != tc.h || math.Abs(m.Duration-6) > 0.1 {
				t.Errorf("meta = %+v", m)
			}
			if res.Mode != "scenes" || strings.Join(frameTimes(res), " ") != "2.00 4.00" {
				t.Errorf("mode %s, times %v; ffmpeg finds cuts at 2.00 4.00", res.Mode, frameTimes(res))
			}
			decodeJPEG(t, res.Frames[0].Data)
		})
	}
}

func TestFragmentedMP4HasTheProgressivePackets(t *testing.T) {
	progressive, err := openMP4Stream(readFixture(t, "cuts.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	fragmented, err := openMP4Stream(readFixture(t, "cuts_fragmented.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(progressive.packets) != len(fragmented.packets) {
		t.Fatalf("%d progressive packets, %d fragmented", len(progressive.packets), len(fragmented.packets))
	}
	for i, p := range progressive.packets {
		f := fragmented.packets[i]
		if p.pts != f.pts || p.sync != f.sync || !bytes.Equal(p.data, f.data) {
			t.Fatalf("packet %d differs: pts %d/%d sync %v/%v", i, p.pts, f.pts, p.sync, f.sync)
		}
	}
	// Without the progressive file's edit list the first frame shows at
	// 0.133 s, so ffprobe places the same cuts at 2.133 and 4.133.
	res, err := ProcessVideo(readFixture(t, "cuts_fragmented.mp4"), ProcessVideoOpts{Frames: 6, Mode: "scenes", SceneThreshold: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(frameTimes(res), " "); got != "2.13 4.13" {
		t.Errorf("fragmented cuts at %s; ffprobe puts them at 2.13 4.13", got)
	}
}

func TestMP4TrackMatrixRotatesFrames(t *testing.T) {
	file := readFixture(t, "cuts.mp4")
	at := bytes.Index(file, []byte("tkhd"))
	if at < 0 || file[at+4] != 0 {
		t.Fatal("fixture needs a version 0 tkhd")
	}
	matrix := at + 4 + 4 + 4*5 + 8 + 8
	for i, v := range []uint32{0, 0x10000, 0, 0xFFFF0000, 0} {
		binary.BigEndian.PutUint32(file[matrix+i*4:], v)
	}
	res, err := ProcessVideo(file, ProcessVideoOpts{Frames: 2, Mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Width != 96 || res.Meta.Height != 160 {
		t.Errorf("a 90° track should present as 96x160, got %dx%d", res.Meta.Width, res.Meta.Height)
	}
}

func TestVP8SamplesKeyframes(t *testing.T) {
	res, err := ProcessVideo(readFixture(t, "vp8.webm"), ProcessVideoOpts{Frames: 6, Mode: "uniform"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Frames {
		if f.TimestampSec != math.Trunc(f.TimestampSec) {
			t.Errorf("frame at %.2f is not a keyframe (one per second)", f.TimestampSec)
		}
	}
	if !strings.Contains(res.Note, "keyframes") {
		t.Errorf("note = %q", res.Note)
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
	_, err := ProcessVideo(data, ProcessVideoOpts{Frames: 3})
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
			_, _ = ProcessVideo(data[:cut], ProcessVideoOpts{Frames: 4, Mode: "scenes", SceneThreshold: 0.3})
		}
		mangled := slices.Clone(data)
		for i := len(mangled) / 4; i < len(mangled); i += 97 {
			mangled[i] ^= 0x5a
		}
		_, _ = ProcessVideo(mangled, ProcessVideoOpts{Frames: 4, Mode: "uniform"})
	}
}
