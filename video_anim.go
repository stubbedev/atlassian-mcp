package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"

	"github.com/kettek/apng"
	"golang.org/x/image/webp"
)

// Animated images (GIF, APNG, animated WebP) as frame streams: frames are
// composited onto a canvas in order, honoring each format's blend and
// disposal rules, so a sampled frame looks as it does in a browser.

// animFrame is one frame before compositing.
type animFrame struct {
	img     image.Image
	rect    image.Rectangle // where it lands on the canvas
	delay   float64         // seconds
	blend   bool            // alpha-blend over the canvas, else replace the rect
	dispose animDispose     // what happens to rect after the frame is shown
}

type animDispose int

const (
	disposeNone animDispose = iota
	disposeBackground
	disposePrevious
)

type animStream struct {
	meta   videoMeta
	frames []animFrame
	starts []float64
	canvas image.Rectangle
}

// openAnimation recognizes an animated image; ok is false for anything else
// (including a single-frame image), so the caller can try other decoders.
func openAnimation(buffer []byte) (s *animStream, ok bool, err error) {
	var frames []animFrame
	var canvas image.Rectangle
	var codec string
	switch {
	case bytes.HasPrefix(buffer, []byte("GIF8")):
		codec = "gif"
		frames, canvas, err = gifFrames(buffer)
	case bytes.HasPrefix(buffer, pngSig) && bytes.Contains(buffer, []byte("acTL")):
		codec = "apng"
		frames, canvas, err = apngFrames(buffer)
	case len(buffer) >= 12 && string(buffer[:4]) == "RIFF" && string(buffer[8:12]) == "WEBP" && bytes.Contains(buffer, []byte("ANMF")):
		codec = "webp"
		frames, canvas, err = webpFrames(buffer)
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if len(frames) == 0 || canvas.Empty() {
		return nil, true, errors.New("animation has no frames")
	}
	s = &animStream{frames: frames, canvas: canvas}
	t := 0.0
	for _, f := range frames {
		s.starts = append(s.starts, t)
		t += f.delay
	}
	s.meta = videoMeta{duration: t, width: canvas.Dx(), height: canvas.Dy(), fps: float64(len(frames)) / t, codec: codec}
	return s, true, nil
}

// browserDelay applies what browsers do with very short frame delays: treat
// them as 100 ms, since many animations were authored assuming that.
func browserDelay(seconds float64) float64 {
	if seconds <= 0.01 {
		return 0.1
	}
	return seconds
}

func gifFrames(buffer []byte) ([]animFrame, image.Rectangle, error) {
	g, err := gif.DecodeAll(bytes.NewReader(buffer))
	if err != nil {
		return nil, image.Rectangle{}, err
	}
	canvas := image.Rect(0, 0, g.Config.Width, g.Config.Height)
	if canvas.Empty() && len(g.Image) > 0 {
		canvas = g.Image[0].Bounds()
	}
	frames := make([]animFrame, len(g.Image))
	for i, img := range g.Image {
		f := animFrame{img: img, rect: img.Bounds(), delay: browserDelay(float64(g.Delay[i]) / 100), blend: true}
		if i < len(g.Disposal) {
			switch g.Disposal[i] {
			case gif.DisposalBackground:
				f.dispose = disposeBackground
			case gif.DisposalPrevious:
				f.dispose = disposePrevious
			}
		}
		frames[i] = f
	}
	return frames, canvas, nil
}

func apngFrames(buffer []byte) ([]animFrame, image.Rectangle, error) {
	a, err := apng.DecodeAll(bytes.NewReader(buffer))
	if err != nil {
		return nil, image.Rectangle{}, err
	}
	var frames []animFrame
	var canvas image.Rectangle
	for i, fr := range a.Frames {
		if i == 0 {
			canvas = image.Rect(0, 0, fr.Image.Bounds().Dx(), fr.Image.Bounds().Dy())
		}
		if fr.IsDefault {
			continue // the static fallback image, not part of the animation
		}
		b := fr.Image.Bounds()
		f := animFrame{
			img:   fr.Image,
			rect:  image.Rect(fr.XOffset, fr.YOffset, fr.XOffset+b.Dx(), fr.YOffset+b.Dy()),
			delay: browserDelay(fr.GetDelay()),
			blend: fr.BlendOp == apng.BLEND_OP_OVER,
		}
		switch fr.DisposeOp {
		case apng.DISPOSE_OP_BACKGROUND:
			f.dispose = disposeBackground
		case apng.DISPOSE_OP_PREVIOUS:
			f.dispose = disposePrevious
			if len(frames) == 0 {
				f.dispose = disposeBackground // per spec, for the first frame
			}
		}
		frames = append(frames, f)
	}
	return frames, canvas, nil
}

// webpFrames demuxes an animated WebP (RIFF chunks VP8X, ANIM, ANMF…) and
// decodes each frame's bitstream as a standalone still WebP.
func webpFrames(buffer []byte) ([]animFrame, image.Rectangle, error) {
	var frames []animFrame
	var canvas image.Rectangle
	err := walkRIFF(buffer[12:], func(id string, data []byte) error {
		switch id {
		case "VP8X":
			if len(data) < 10 {
				return errors.New("short VP8X chunk")
			}
			canvas = image.Rect(0, 0, int(u24(data[4:]))+1, int(u24(data[7:]))+1)
		case "ANMF":
			if len(data) < 16 {
				return errors.New("short ANMF chunk")
			}
			x, y := int(u24(data[0:]))*2, int(u24(data[3:]))*2
			w, h := int(u24(data[6:]))+1, int(u24(data[9:]))+1
			flags := data[15]
			img, err := decodeWebPFrame(data[16:], w, h)
			if err != nil {
				return err
			}
			f := animFrame{
				img:   img,
				rect:  image.Rect(x, y, x+w, y+h),
				delay: browserDelay(float64(u24(data[12:])) / 1000),
				blend: flags&0x02 == 0,
			}
			if flags&0x01 != 0 {
				f.dispose = disposeBackground
			}
			frames = append(frames, f)
		}
		return nil
	})
	return frames, canvas, err
}

func u24(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 }

func walkRIFF(data []byte, fn func(id string, data []byte) error) error {
	for len(data) >= 8 {
		id := string(data[:4])
		size := int(binary.LittleEndian.Uint32(data[4:8]))
		if size < 0 || 8+size > len(data) {
			return errors.New("truncated RIFF chunk " + id)
		}
		if err := fn(id, data[8:8+size]); err != nil {
			return err
		}
		data = data[8+size+size%2:]
	}
	return nil
}

// decodeWebPFrame wraps an ANMF payload (optional ALPH, then VP8 or VP8L)
// into a still WebP file x/image/webp can read.
func decodeWebPFrame(payload []byte, w, h int) (image.Image, error) {
	var body bytes.Buffer
	body.WriteString("WEBP")
	if bytes.HasPrefix(payload, []byte("ALPH")) {
		vp8x := make([]byte, 10)
		vp8x[0] = 0x10                // alpha
		putU24(vp8x[4:], uint32(w-1)) //nolint:gosec // frame sizes are 24-bit in WebP
		putU24(vp8x[7:], uint32(h-1)) //nolint:gosec // frame sizes are 24-bit in WebP
		writeChunk(&body, "VP8X", vp8x)
	}
	body.Write(payload)
	var file bytes.Buffer
	file.WriteString("RIFF")
	_ = binary.Write(&file, binary.LittleEndian, uint32(body.Len())) //nolint:gosec // bounded by the input size
	file.Write(body.Bytes())
	return webp.Decode(&file)
}

func putU24(b []byte, v uint32) { b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16) } //nolint:gosec // truncation is the point

func writeChunk(w *bytes.Buffer, id string, data []byte) {
	w.WriteString(id)
	_ = binary.Write(w, binary.LittleEndian, uint32(len(data))) //nolint:gosec // chunk sizes are 32-bit
	w.Write(data)
	if len(data)%2 == 1 {
		w.WriteByte(0)
	}
}

func (s *animStream) info() videoMeta    { return s.meta }
func (s *animStream) times() []float64   { return s.starts }
func (s *animStream) segments() [][2]int { return [][2]int{{0, len(s.frames)}} }

// pixels is small: compositing costs a fraction of video decoding.
func (s *animStream) pixels() int { return max(1, s.canvas.Dx()*s.canvas.Dy()/20) }

func (s *animStream) decodeSegment(_, last int, visit func(i int, p picture) bool) error {
	canvas := image.NewNRGBA(s.canvas)
	var saved *image.NRGBA
	for i, f := range s.frames {
		if f.dispose == disposePrevious {
			saved = image.NewNRGBA(f.rect)
			draw.Draw(saved, f.rect, canvas, f.rect.Min, draw.Src)
		}
		op := draw.Src
		if f.blend {
			op = draw.Over
		}
		draw.Draw(canvas, f.rect, f.img, f.img.Bounds().Min, op)
		if !visit(i, rgbPicture{canvas}) || i >= last {
			return nil
		}
		switch f.dispose {
		case disposeBackground:
			draw.Draw(canvas, f.rect, image.Transparent, image.Point{}, draw.Src)
		case disposePrevious:
			draw.Draw(canvas, f.rect, saved, f.rect.Min, draw.Src)
		}
	}
	return nil
}

// rgbPicture is a composited frame. It is only valid during the visit call.
type rgbPicture struct{ img *image.NRGBA }

// image flattens transparency onto white, which is how an animation with a
// transparent background reads on a page.
func (p rgbPicture) image() image.Image {
	out := image.NewNRGBA(p.img.Rect)
	draw.Draw(out, out.Rect, image.White, image.Point{}, draw.Src)
	draw.Draw(out, out.Rect, p.img, p.img.Rect.Min, draw.Over)
	return out
}

func (p rgbPicture) planes(scale int) framePlanes {
	img := p.image().(*image.NRGBA)
	w, h := img.Rect.Dx(), img.Rect.Dy()
	ys, cbs, crs := newPlane(w, h), newPlane(w, h), newPlane(w, h)
	for y := range h {
		for x := range w {
			o := y*img.Stride + x*4
			ys.pix[y*w+x], cbs.pix[y*w+x], crs.pix[y*w+x] = color.RGBToYCbCr(img.Pix[o], img.Pix[o+1], img.Pix[o+2])
		}
	}
	return framePlanes{boxDownscale(ys, scale), boxDownscale(cbs, scale), boxDownscale(crs, scale)}
}
