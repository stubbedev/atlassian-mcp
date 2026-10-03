package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"slices"

	"github.com/disintegration/imaging"
	"github.com/thesyncim/goh264"
	"golang.org/x/image/vp8"
)

// A container demuxer (MP4/MOV, Matroska/WebM, AVI) reduces a file to the
// video track's compressed packets in decode order plus their timing, and
// names a codec to decode them with. packetStream does the rest the same way
// for every container: GOPs as independently decodable segments, display
// order, timestamps.

type videoPacket struct {
	data []byte
	pts  int64 // presentation time, in the stream's timescale
	sync bool  // decodable without any earlier packet
}

type videoCodec struct {
	name string // as ffprobe reports it
	// keyframesOnly marks a codec decoded from sync packets alone, because
	// only its intra frames have a pure-Go decoder; the stream then offers
	// just those frames.
	keyframesOnly bool
	newDecoder    func() (packetDecoder, error)
}

type packetDecoder interface {
	// decode takes one packet and returns the pictures it completes, in
	// display order (a codec with B-frames may return none yet).
	decode(data []byte) ([]picture, error)
	// flush returns pictures still held for reordering.
	flush() ([]picture, error)
}

// streamTiming maps packet timestamps to seconds on the presentation
// timeline: (pts - shift) / timescale + delay. A packet before shift is
// decoded as a reference but never shown.
type streamTiming struct {
	timescale float64
	shift     int64
	delay     float64
	duration  float64 // seconds; 0 when the container does not say
}

type packetStream struct {
	meta     videoMeta
	codec    videoCodec
	packets  []videoPacket // decode order
	gops     [][2]int      // decode-order packet ranges [lo, hi)
	segs     [][2]int      // the same GOPs as display-index ranges
	display  []float64     // presentation time per display index
	rotation int           // clockwise degrees to rotate decoded frames for display
	px       int
}

func newPacketStream(packets []videoPacket, codec videoCodec, timing streamTiming, width, height, rotation int) (*packetStream, error) {
	allFrames := len(packets)
	if codec.keyframesOnly {
		packets = slices.DeleteFunc(slices.Clone(packets), func(p videoPacket) bool { return !p.sync })
	}
	if len(packets) == 0 {
		return nil, errors.New("video track has no decodable frames")
	}
	if timing.timescale <= 0 {
		return nil, errors.New("video track has no timescale")
	}
	s := &packetStream{codec: codec, packets: packets, rotation: rotation, px: max(1, width*height)}

	// GOPs in decode order; a stream that opens without a sync packet still
	// decodes from its first one.
	start := 0
	for i := 1; i < len(packets); i++ {
		if packets[i].sync {
			s.gops = append(s.gops, [2]int{start, i})
			start = i
		}
	}
	s.gops = append(s.gops, [2]int{start, len(packets)})

	for _, g := range s.gops {
		pts := make([]int64, 0, g[1]-g[0])
		for _, p := range packets[g[0]:g[1]] {
			pts = append(pts, p.pts)
		}
		slices.Sort(pts)
		lo := len(s.display)
		for _, p := range pts {
			t := float64(p-timing.shift)/timing.timescale + timing.delay
			if p < timing.shift {
				t = -1
			}
			s.display = append(s.display, t)
		}
		s.segs = append(s.segs, [2]int{lo, len(s.display)})
	}

	shown, last := 0, 0.0
	for _, t := range s.display {
		if t >= 0 {
			shown++
			last = max(last, t)
		}
	}
	duration := timing.duration
	if duration <= 0 {
		// One frame's worth past the last one shown.
		duration = last + last/float64(max(1, shown-1))
	}
	if duration <= 0 {
		duration = 1
	}
	if rotation == 90 || rotation == 270 {
		width, height = height, width
	}
	if codec.keyframesOnly {
		shown = allFrames
	}
	s.meta = videoMeta{duration: duration, width: width, height: height, fps: float64(shown) / duration, codec: codec.name}
	return s, nil
}

func (s *packetStream) info() videoMeta    { return s.meta }
func (s *packetStream) times() []float64   { return s.display }
func (s *packetStream) segments() [][2]int { return s.segs }
func (s *packetStream) pixels() int        { return s.px }

func (s *packetStream) note() string {
	if s.codec.keyframesOnly {
		return fmt.Sprintf("Only %s keyframes can be decoded, so frames come from the nearest keyframes.", s.codec.name)
	}
	return ""
}

func (s *packetStream) decodeSegment(seg, last int, visit func(i int, p picture) bool) error {
	dec, err := s.codec.newDecoder()
	if err != nil {
		return err
	}
	g := s.gops[seg]
	next := s.segs[seg][0]
	emit := func(pics []picture) bool {
		for _, p := range pics {
			if next >= s.segs[seg][1] {
				return false
			}
			if s.rotation != 0 {
				p = rotatedPicture{p, s.rotation}
			}
			i := next
			next++
			if !visit(i, p) || i >= last {
				return false
			}
		}
		return true
	}
	for _, pkt := range s.packets[g[0]:g[1]] {
		pics, err := dec.decode(pkt.data)
		if err != nil {
			return fmt.Errorf("%s decode failed: %w", s.codec.name, err)
		}
		if !emit(pics) {
			return nil
		}
	}
	pics, err := dec.flush()
	if err != nil {
		return fmt.Errorf("%s decode failed: %w", s.codec.name, err)
	}
	emit(pics)
	return nil
}

// rotatedPicture applies a container's display rotation. Comparisons work
// on the unrotated planes, which compare the same.
type rotatedPicture struct {
	picture

	degrees int
}

func (p rotatedPicture) image() image.Image {
	img := p.picture.image()
	switch p.degrees {
	case 90:
		return imaging.Rotate270(img) // imaging rotates counter-clockwise
	case 180:
		return imaging.Rotate180(img)
	case 270:
		return imaging.Rotate90(img)
	}
	return img
}

// ── H.264 ────────────────────────────────────────────────────────────────────

// h264AVCC decodes length-prefixed packets configured by an avcC record, as
// MP4 and Matroska store H.264.
func h264AVCC(avcC []byte) (videoCodec, int, int, error) {
	info, err := goh264.InspectAVCC(avcC)
	if err != nil {
		return videoCodec{}, 0, 0, fmt.Errorf("invalid H.264 configuration: %w", err)
	}
	codec := videoCodec{name: "h264", newDecoder: func() (packetDecoder, error) {
		d := goh264.NewDecoder()
		if _, err := d.ConfigureAVCC(avcC); err != nil {
			return nil, fmt.Errorf("invalid H.264 configuration: %w", err)
		}
		return &h264Decoder{d: d}, nil
	}}
	return codec, info.StreamInfo.Width, info.StreamInfo.Height, nil
}

// h264AnnexB decodes start-code delimited packets, as AVI stores H.264.
// paramSets (SPS/PPS NAL units, start codes included) are fed ahead of each
// GOP, since an AVI usually carries them only once, before its first frame.
func h264AnnexB(paramSets []byte) (videoCodec, int, int, error) {
	info, err := goh264.InspectAnnexBHeaders(paramSets)
	if err != nil {
		return videoCodec{}, 0, 0, fmt.Errorf("H.264 stream has no usable SPS/PPS: %w", err)
	}
	codec := videoCodec{name: "h264", newDecoder: func() (packetDecoder, error) {
		return &h264Decoder{d: goh264.NewDecoder(), annexB: true, prefix: paramSets}, nil
	}}
	return codec, info.Width, info.Height, nil
}

type h264Decoder struct {
	d      *goh264.Decoder
	annexB bool
	prefix []byte // fed once, ahead of the first packet
}

func (h *h264Decoder) decode(data []byte) ([]picture, error) {
	var frames []*goh264.Frame
	var err error
	if h.annexB {
		if h.prefix != nil {
			data = append(slices.Clip(h.prefix), data...)
			h.prefix = nil
		}
		frames, err = h.d.DecodeFrames(data)
	} else {
		frames, err = h.d.DecodeConfiguredAVCFrames(data)
	}
	return yuvPictures(frames), err
}

func (h *h264Decoder) flush() ([]picture, error) {
	frames, err := h.d.FlushDelayedFrames()
	return yuvPictures(frames), err
}

func yuvPictures(frames []*goh264.Frame) []picture {
	out := make([]picture, len(frames))
	for i, f := range frames {
		out[i] = &yuvPicture{f: f}
	}
	return out
}

// annexBNALs splits an Annex-B byte stream into NAL units, start codes removed.
func annexBNALs(b []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		if start >= 0 {
			end := i
			if end > start && b[end-1] == 0 {
				end-- // the leading zero of a 4-byte start code
			}
			nals = append(nals, b[start:end])
		}
		start = i + 3
		i += 2
	}
	if start >= 0 && start < len(b) {
		nals = append(nals, b[start:])
	}
	return nals
}

// scanAnnexB reports whether a packet holds an IDR slice and collects the
// newest SPS/PPS it carries into params.
func scanAnnexB(pkt []byte, params map[byte][]byte) (idr bool) {
	for _, nal := range annexBNALs(pkt) {
		if len(nal) == 0 {
			continue
		}
		switch t := nal[0] & 0x1f; t {
		case 5:
			idr = true
		case 7, 8:
			params[t] = nal
		}
	}
	return idr
}

func joinParamSets(params map[byte][]byte) []byte {
	var b []byte
	for _, t := range []byte{7, 8} {
		if nal, ok := params[t]; ok {
			b = append(b, 0, 0, 0, 1)
			b = append(b, nal...)
		}
	}
	return b
}

// ── Motion JPEG ──────────────────────────────────────────────────────────────

var mjpegCodec = videoCodec{name: "mjpeg", newDecoder: func() (packetDecoder, error) { return mjpegDecoder{}, nil }}

type mjpegDecoder struct{}

func (mjpegDecoder) decode(data []byte) ([]picture, error) {
	img, err := jpeg.Decode(bytes.NewReader(withDefaultHuffmanTables(data)))
	if err != nil {
		return nil, err
	}
	return []picture{imagePicture{img}}, nil
}

func (mjpegDecoder) flush() ([]picture, error) { return nil, nil }

// withDefaultHuffmanTables inserts the standard JPEG Huffman tables (ITU
// T.81 K.3) before the first scan when a frame has none: Motion JPEG as
// AVI and many cameras write it leaves them out to save space.
func withDefaultHuffmanTables(data []byte) []byte {
	for i := 2; i+4 <= len(data) && data[i] == 0xff; {
		marker := data[i+1]
		switch {
		case marker == 0xc4: // DHT present
			return data
		case marker == 0xda: // start of scan, no DHT seen
			out := make([]byte, 0, len(data)+len(defaultDHT))
			out = append(out, data[:i]...)
			out = append(out, defaultDHT...)
			return append(out, data[i:]...)
		case marker >= 0xd0 && marker <= 0xd9:
			i += 2
		default:
			i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
		}
	}
	return data
}

// defaultDHT is one DHT segment holding the four standard tables.
var defaultDHT = func() []byte {
	tables := []struct {
		class   byte
		counts  [16]byte
		symbols []byte
	}{
		{0x00, [16]byte{0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}},
		{0x01, [16]byte{0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}},
		{0x10, [16]byte{0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7d}, []byte{
			0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
			0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
			0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
			0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
			0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
			0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
			0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
			0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
			0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
			0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
			0xf9, 0xfa,
		}},
		{0x11, [16]byte{0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}, []byte{
			0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
			0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91, 0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
			0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
			0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
			0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
			0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
			0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
			0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
			0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
			0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
			0xf9, 0xfa,
		}},
	}
	var body []byte
	for _, t := range tables {
		body = append(body, t.class)
		body = append(body, t.counts[:]...)
		body = append(body, t.symbols...)
	}
	n := len(body) + 2
	return append([]byte{0xff, 0xc4, byte(n >> 8), byte(n)}, body...)
}()

// ── VP8 ──────────────────────────────────────────────────────────────────────

// vp8Codec decodes keyframes only: x/image/vp8 (the decoder behind WebP)
// has no inter prediction.
var vp8Codec = videoCodec{name: "vp8", keyframesOnly: true, newDecoder: func() (packetDecoder, error) {
	return &vp8Decoder{d: vp8.NewDecoder()}, nil
}}

type vp8Decoder struct{ d *vp8.Decoder }

func (v *vp8Decoder) decode(data []byte) ([]picture, error) {
	v.d.Init(bytes.NewReader(data), len(data))
	fh, err := v.d.DecodeFrameHeader()
	if err != nil {
		return nil, err
	}
	if !fh.KeyFrame {
		return nil, nil
	}
	img, err := v.d.DecodeFrame()
	if err != nil {
		return nil, err
	}
	return []picture{imagePicture{img}}, nil
}

func (v *vp8Decoder) flush() ([]picture, error) { return nil, nil }

// ── decoded still pictures ───────────────────────────────────────────────────

// imagePicture is a frame some image decoder returned whole.
type imagePicture struct{ img image.Image }

func (p imagePicture) image() image.Image { return p.img }

func (p imagePicture) planes(scale int) framePlanes {
	if yc, ok := p.img.(*image.YCbCr); ok {
		w, h := yc.Rect.Dx(), yc.Rect.Dy()
		cw, ch := w, h
		switch yc.SubsampleRatio {
		case image.YCbCrSubsampleRatio420:
			cw, ch = (w+1)/2, (h+1)/2
		case image.YCbCrSubsampleRatio422:
			cw = (w + 1) / 2
		case image.YCbCrSubsampleRatio440:
			ch = (h + 1) / 2
		case image.YCbCrSubsampleRatio411:
			cw = (w + 3) / 4
		case image.YCbCrSubsampleRatio410:
			cw, ch = (w+3)/4, (h+1)/2
		}
		cs := max(1, scale*cw/max(1, w))
		return framePlanes{
			boxDownscale(plane{pix: yc.Y[yc.YOffset(yc.Rect.Min.X, yc.Rect.Min.Y):], w: w, h: h, stride: yc.YStride}, scale),
			boxDownscale(plane{pix: yc.Cb[yc.COffset(yc.Rect.Min.X, yc.Rect.Min.Y):], w: cw, h: ch, stride: yc.CStride}, cs),
			boxDownscale(plane{pix: yc.Cr[yc.COffset(yc.Rect.Min.X, yc.Rect.Min.Y):], w: cw, h: ch, stride: yc.CStride}, cs),
		}
	}
	b := p.img.Bounds()
	w, h := b.Dx(), b.Dy()
	ys, cbs, crs := newPlane(w, h), newPlane(w, h), newPlane(w, h)
	for y := range h {
		for x := range w {
			r, g, bl, _ := p.img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			ys.pix[y*w+x], cbs.pix[y*w+x], crs.pix[y*w+x] = color.RGBToYCbCr(uint8(r>>8), uint8(g>>8), uint8(bl>>8)) //nolint:gosec // 16-bit channels shifted to 8
		}
	}
	return framePlanes{boxDownscale(ys, scale), boxDownscale(cbs, scale), boxDownscale(crs, scale)}
}
