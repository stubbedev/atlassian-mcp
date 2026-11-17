package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/thesyncim/goh264"
)

// mp4Sample is a sample located in the file.
type mp4Sample = videoPacket

// openMP4Stream demuxes the video track of an MP4 or QuickTime file:
// H.264, or Motion JPEG as cameras and some screen tools write it.
func openMP4Stream(buffer []byte) (*packetStream, error) {
	mov, err := parseMP4(buffer)
	if err != nil || !mov.hasMoov {
		return nil, errUnsupportedVideo
	}
	var trak *mp4Track
	for i := range mov.tracks {
		if mov.tracks[i].handler == "vide" {
			trak = &mov.tracks[i]
			break
		}
	}
	if trak == nil {
		return nil, errors.New("No video stream found.")
	}
	if trak.entry == "" {
		return nil, errors.New("video track has no sample description")
	}
	var codec videoCodec
	w, h := trak.width, trak.height
	switch {
	case (trak.entry == "avc1" || trak.entry == "avc3") && trak.avcC != nil:
		var cw, ch int
		if codec, cw, ch, err = h264AVCC(trak.avcC); err != nil {
			return nil, err
		}
		if cw != 0 {
			w, h = cw, ch
		}
	case trak.entry == "jpeg" || trak.entry == "mjpa":
		codec = mjpegCodec
	default:
		return nil, unsupportedCodec(trak.entry)
	}

	var packets []videoPacket
	if len(mov.fragments) > 0 {
		packets, err = fragmentedSamples(mov, trak.id, buffer)
	} else {
		packets, err = progressiveSamples(&trak.stbl, buffer)
	}
	if err != nil {
		return nil, err
	}
	// The edit list's first media edit says which media time is shown at 0
	// and how long the movie is; an empty edit before it delays the start.
	shift, delay, duration := editList(trak.edits, mov.timescale)
	timescale := float64(trak.timescale)
	if duration <= 0 && timescale > 0 {
		duration = float64(trak.duration) / timescale
	}
	timing := streamTiming{timescale: timescale, shift: shift, delay: delay, duration: duration}
	return newPacketStream(packets, codec, timing, w, h, trackRotation(trak.matrixA, trak.matrixB))
}

// ── box parsing ──────────────────────────────────────────────────────────────

type mp4Movie struct {
	hasMoov   bool
	timescale uint32
	tracks    []mp4Track
	trex      map[uint32]mp4Trex
	fragments []mp4Fragment
}

type mp4Track struct {
	id               uint32
	handler          string
	matrixA, matrixB int32
	edits            []mp4Edit
	timescale        uint32
	duration         uint64
	entry            string
	width, height    int
	avcC             []byte
	stbl             mp4SampleTable
}

type mp4Edit struct {
	segmentDuration uint64
	mediaTime       int64
}

type mp4SampleTable struct {
	sampleSize   uint32
	sampleCount  uint32
	sizes        []byte
	chunkOffsets []uint64
	stsc         []mp4ChunkRun
	stts         []mp4Run
	ctts         []mp4Run
	sync         map[uint32]bool
	hasSizes     bool
	hasStsc      bool
	hasStts      bool
	hasOffsets   bool
}

type mp4ChunkRun struct{ firstChunk, samplesPerChunk uint32 }

type mp4Run struct {
	count uint32
	value int64
}

type mp4Trex struct{ duration, size, flags uint32 }

type mp4Fragment struct {
	start int
	trafs []mp4Traf
}

type mp4Traf struct {
	trackID                                 uint32
	baseOffset                              uint64
	hasBaseOffset                           bool
	defDuration, defSize, defFlags          uint32
	hasDefDuration, hasDefSize, hasDefFlags bool
	tfdt                                    uint64
	hasTfdt                                 bool
	truns                                   []mp4Trun
}

type mp4Trun struct {
	dataOffset    int32
	hasDataOffset bool
	samples       []mp4TrunSample
}

type mp4TrunSample struct {
	duration, size, flags          uint32
	hasDuration, hasSize, hasFlags bool
	cto                            int64
}

var errMP4Box = errors.New("MP4 box runs past its parent")

// walkBoxes visits the boxes in b, passing each one's offset within b.
func walkBoxes(b []byte, visit func(kind string, payload []byte, at int) error) error {
	for i := 0; i < len(b); {
		rest := uint64(len(b) - i)
		if rest < 8 {
			return errMP4Box
		}
		size := uint64(binary.BigEndian.Uint32(b[i:]))
		kind := string(b[i+4 : i+8])
		header := 8
		switch size {
		case 0:
			size = rest
		case 1:
			if rest < 16 {
				return errMP4Box
			}
			size = binary.BigEndian.Uint64(b[i+8:])
			header = 16
		}
		if size < uint64(header) || size > rest {
			return errMP4Box
		}
		end := i + int(size) //nolint:gosec // size is at most len(b)-i, checked above
		if err := visit(kind, b[i+header:end], i); err != nil {
			return err
		}
		i = end
	}
	return nil
}

type mp4Reader struct {
	b   []byte
	err error
}

func (r *mp4Reader) take(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errMP4Box
		return make([]byte, max(n, 0))
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *mp4Reader) u32() uint32 { return binary.BigEndian.Uint32(r.take(4)) }
func (r *mp4Reader) u64() uint64 { return binary.BigEndian.Uint64(r.take(8)) }

func (r *mp4Reader) fullBox() (version uint8, flags uint32) {
	v := r.u32()
	return uint8(v >> 24), v & 0xFFFFFF
}

func (r *mp4Reader) uv(version uint8) uint64 {
	if version == 1 {
		return r.u64()
	}
	return uint64(r.u32())
}

func parseMP4(buffer []byte) (*mp4Movie, error) {
	mov := &mp4Movie{trex: map[uint32]mp4Trex{}}
	err := walkBoxes(buffer, func(kind string, payload []byte, at int) error {
		switch kind {
		case "moov":
			mov.hasMoov = true
			return parseMoov(mov, payload)
		case "moof":
			frag, err := parseMoof(payload, at)
			if err != nil {
				return fmt.Errorf("movie fragment at %d: %w", at, err)
			}
			mov.fragments = append(mov.fragments, frag)
		}
		return nil
	})
	return mov, err
}

func parseMoov(mov *mp4Movie, payload []byte) error {
	return walkBoxes(payload, func(kind string, b []byte, _ int) error {
		switch kind {
		case "mvhd":
			r := &mp4Reader{b: b}
			v, _ := r.fullBox()
			r.uv(v)
			r.uv(v)
			mov.timescale = r.u32()
			return r.err
		case "trak":
			t, err := parseTrak(b)
			if err != nil {
				return err
			}
			mov.tracks = append(mov.tracks, t)
		case "mvex":
			return walkBoxes(b, func(kind string, b []byte, _ int) error {
				if kind != "trex" {
					return nil
				}
				r := &mp4Reader{b: b}
				r.fullBox()
				id := r.u32()
				r.u32()
				mov.trex[id] = mp4Trex{duration: r.u32(), size: r.u32(), flags: r.u32()}
				return r.err
			})
		}
		return nil
	})
}

func parseTrak(payload []byte) (mp4Track, error) {
	var t mp4Track
	err := walkBoxes(payload, func(kind string, b []byte, _ int) error {
		switch kind {
		case "tkhd":
			r := &mp4Reader{b: b}
			v, _ := r.fullBox()
			r.uv(v)
			r.uv(v)
			t.id = r.u32()
			r.u32()
			r.uv(v)
			r.take(16)
			t.matrixA = int32(r.u32()) //nolint:gosec // the matrix holds signed 16.16 values
			t.matrixB = int32(r.u32()) //nolint:gosec // the matrix holds signed 16.16 values
			return r.err
		case "edts":
			return walkBoxes(b, func(kind string, b []byte, _ int) error {
				if kind != "elst" || t.edits != nil {
					return nil
				}
				r := &mp4Reader{b: b}
				v, _ := r.fullBox()
				n := r.u32()
				for range n {
					if r.err != nil {
						break
					}
					e := mp4Edit{segmentDuration: r.uv(v)}
					if v == 1 {
						e.mediaTime = int64(r.u64()) //nolint:gosec // media_time is signed
					} else {
						e.mediaTime = int64(int32(r.u32())) //nolint:gosec // media_time is signed
					}
					r.u32()
					t.edits = append(t.edits, e)
				}
				if t.edits == nil {
					t.edits = []mp4Edit{}
				}
				return r.err
			})
		case "mdia":
			return parseMdia(&t, b)
		}
		return nil
	})
	return t, err
}

func parseMdia(t *mp4Track, payload []byte) error {
	return walkBoxes(payload, func(kind string, b []byte, _ int) error {
		switch kind {
		case "mdhd":
			r := &mp4Reader{b: b}
			v, _ := r.fullBox()
			r.uv(v)
			r.uv(v)
			t.timescale = r.u32()
			t.duration = r.uv(v)
			return r.err
		case "hdlr":
			r := &mp4Reader{b: b}
			r.fullBox()
			r.u32()
			t.handler = string(r.take(4))
			return r.err
		case "minf":
			return walkBoxes(b, func(kind string, b []byte, _ int) error {
				if kind != "stbl" {
					return nil
				}
				return parseStbl(t, b)
			})
		}
		return nil
	})
}

const visualSampleEntryHeader = 78

func parseStbl(t *mp4Track, payload []byte) error {
	st := &t.stbl
	return walkBoxes(payload, func(kind string, b []byte, _ int) error {
		r := &mp4Reader{b: b}
		switch kind {
		case "stsd":
			r.fullBox()
			if r.u32() == 0 {
				return r.err
			}
			return walkBoxes(r.b, func(kind string, entry []byte, _ int) error {
				if t.entry != "" {
					return nil
				}
				t.entry = kind
				if len(entry) < visualSampleEntryHeader {
					return nil
				}
				t.width = int(binary.BigEndian.Uint16(entry[24:]))
				t.height = int(binary.BigEndian.Uint16(entry[26:]))
				return walkBoxes(entry[visualSampleEntryHeader:], func(kind string, b []byte, _ int) error {
					if kind == "avcC" {
						t.avcC = b
					}
					return nil
				})
			})
		case "stsz":
			r.fullBox()
			st.sampleSize = r.u32()
			st.sampleCount = r.u32()
			if st.sampleSize == 0 {
				st.sizes = r.take(int(st.sampleCount) * 4)
			}
			st.hasSizes = true
		case "stsc":
			r.fullBox()
			n := r.u32()
			for range n {
				if r.err != nil {
					break
				}
				run := mp4ChunkRun{firstChunk: r.u32(), samplesPerChunk: r.u32()}
				r.u32()
				st.stsc = append(st.stsc, run)
			}
			st.hasStsc = true
		case "stco", "co64":
			r.fullBox()
			n := r.u32()
			for range n {
				if r.err != nil {
					break
				}
				if kind == "co64" {
					st.chunkOffsets = append(st.chunkOffsets, r.u64())
				} else {
					st.chunkOffsets = append(st.chunkOffsets, uint64(r.u32()))
				}
			}
			st.hasOffsets = true
		case "stts", "ctts":
			r.fullBox()
			n := r.u32()
			runs := make([]mp4Run, 0, min(int(n), len(r.b)/8))
			for range n {
				if r.err != nil {
					break
				}
				count := r.u32()
				runs = append(runs, mp4Run{count: count, value: int64(int32(r.u32()))}) //nolint:gosec // ctts offsets are signed in practice
			}
			if kind == "stts" {
				st.stts, st.hasStts = runs, true
			} else {
				st.ctts = runs
			}
		case "stss":
			r.fullBox()
			n := r.u32()
			st.sync = make(map[uint32]bool, min(int(n), len(r.b)/4))
			for range n {
				if r.err != nil {
					break
				}
				st.sync[r.u32()] = true
			}
		}
		return r.err
	})
}

func parseMoof(payload []byte, start int) (mp4Fragment, error) {
	frag := mp4Fragment{start: start}
	err := walkBoxes(payload, func(kind string, b []byte, _ int) error {
		if kind != "traf" {
			return nil
		}
		var traf mp4Traf
		err := walkBoxes(b, func(kind string, b []byte, _ int) error {
			r := &mp4Reader{b: b}
			switch kind {
			case "tfhd":
				_, flags := r.fullBox()
				traf.trackID = r.u32()
				if flags&0x1 != 0 {
					traf.baseOffset, traf.hasBaseOffset = r.u64(), true
				}
				if flags&0x2 != 0 {
					r.u32()
				}
				if flags&0x8 != 0 {
					traf.defDuration, traf.hasDefDuration = r.u32(), true
				}
				if flags&0x10 != 0 {
					traf.defSize, traf.hasDefSize = r.u32(), true
				}
				if flags&0x20 != 0 {
					traf.defFlags, traf.hasDefFlags = r.u32(), true
				}
			case "tfdt":
				v, _ := r.fullBox()
				traf.tfdt, traf.hasTfdt = r.uv(v), true
			case "trun":
				v, flags := r.fullBox()
				n := r.u32()
				var trun mp4Trun
				if flags&0x1 != 0 {
					trun.dataOffset, trun.hasDataOffset = int32(r.u32()), true //nolint:gosec // data_offset is signed
				}
				var firstFlags uint32
				hasFirstFlags := flags&0x4 != 0
				if hasFirstFlags {
					firstFlags = r.u32()
				}
				trun.samples = make([]mp4TrunSample, 0, min(int(n), len(r.b)/4+1))
				for i := range n {
					if r.err != nil {
						break
					}
					var smp mp4TrunSample
					if flags&0x100 != 0 {
						smp.duration, smp.hasDuration = r.u32(), true
					}
					if flags&0x200 != 0 {
						smp.size, smp.hasSize = r.u32(), true
					}
					if flags&0x400 != 0 {
						smp.flags, smp.hasFlags = r.u32(), true
					} else if i == 0 && hasFirstFlags {
						smp.flags, smp.hasFlags = firstFlags, true
					}
					if flags&0x800 != 0 {
						if v == 0 {
							smp.cto = int64(r.u32())
						} else {
							smp.cto = int64(int32(r.u32())) //nolint:gosec // version 1 offsets are signed
						}
					}
					trun.samples = append(trun.samples, smp)
				}
				traf.truns = append(traf.truns, trun)
			}
			return r.err
		})
		if err != nil {
			return err
		}
		frag.trafs = append(frag.trafs, traf)
		return nil
	})
	return frag, err
}

// ── samples ──────────────────────────────────────────────────────────────────

func expandRuns(runs []mp4Run, n int) ([]int64, error) {
	out := make([]int64, 0, n)
	for _, run := range runs {
		for range run.count {
			if len(out) == n {
				return out, nil
			}
			out = append(out, run.value)
		}
	}
	if len(out) < n {
		return nil, fmt.Errorf("sample table covers %d of %d samples", len(out), n)
	}
	return out, nil
}

// progressiveSamples walks a classic sample table: chunk offsets, samples
// per chunk, sizes, decode times, composition offsets and sync samples.
func progressiveSamples(st *mp4SampleTable, buffer []byte) ([]mp4Sample, error) {
	if !st.hasSizes || !st.hasStsc || !st.hasStts {
		return nil, errors.New("video track is missing its sample table")
	}
	if !st.hasOffsets {
		return nil, errors.New("video track has no chunk offsets")
	}
	n := int(st.sampleCount)
	if st.sampleSize == 0 && len(st.sizes) < n*4 {
		return nil, errors.New("video track's sample size table is truncated")
	}
	var cts []int64
	if st.ctts != nil {
		var err error
		if cts, err = expandRuns(st.ctts, n); err != nil {
			return nil, err
		}
	}
	durs, err := expandRuns(st.stts, n)
	if err != nil {
		return nil, err
	}
	sizeOf := func(i int) uint64 {
		if st.sampleSize != 0 {
			return uint64(st.sampleSize)
		}
		return uint64(binary.BigEndian.Uint32(st.sizes[i*4:]))
	}
	samples := make([]mp4Sample, 0, n)
	var dts int64
	for e, entry := range st.stsc {
		lastChunk := uint32(len(st.chunkOffsets)) //nolint:gosec // chunk counts are 32-bit in MP4
		if e+1 < len(st.stsc) {
			lastChunk = st.stsc[e+1].firstChunk - 1
		}
		for chunk := entry.firstChunk; chunk <= lastChunk && len(samples) < n; chunk++ {
			if int(chunk) > len(st.chunkOffsets) || chunk == 0 {
				return nil, errors.New("corrupt sample-to-chunk table")
			}
			off := st.chunkOffsets[chunk-1]
			for range entry.samplesPerChunk {
				i := len(samples)
				if i >= n {
					break
				}
				size := sizeOf(i)
				if off+size > uint64(len(buffer)) {
					return nil, errors.New("video sample lies outside the file (truncated download?)")
				}
				pts := dts
				if cts != nil {
					pts += cts[i]
				}
				sync := st.sync == nil || st.sync[uint32(i+1)] //nolint:gosec // sample counts are 32-bit in MP4
				samples = append(samples, mp4Sample{data: buffer[off : off+size], pts: pts, sync: sync})
				dts += durs[i]
				off += size
			}
		}
	}
	return samples, nil
}

const sampleIsNonSync = 0x00010000

// fragmentedSamples collects a track's samples from every movie fragment,
// as written by recorders that stream (e.g. Safari's MediaRecorder).
func fragmentedSamples(mov *mp4Movie, trackID uint32, buffer []byte) ([]mp4Sample, error) {
	trex := mov.trex[trackID]
	var samples []mp4Sample
	var nextDTS uint64
	for _, frag := range mov.fragments {
		for _, traf := range frag.trafs {
			if traf.trackID != trackID {
				continue
			}
			base := uint64(frag.start) //nolint:gosec // a file offset
			if traf.hasBaseOffset {
				base = traf.baseOffset
			}
			dts := nextDTS
			if traf.hasTfdt {
				dts = traf.tfdt
			}
			var off uint64
			for k, trun := range traf.truns {
				if trun.hasDataOffset || k == 0 {
					off = uint64(int64(base) + int64(trun.dataOffset)) //nolint:gosec // offsets within the file
				}
				for _, smp := range trun.samples {
					dur, size, flags := smp.duration, smp.size, smp.flags
					if !smp.hasDuration {
						dur = pick(traf.hasDefDuration, traf.defDuration, trex.duration)
					}
					if !smp.hasSize {
						size = pick(traf.hasDefSize, traf.defSize, trex.size)
					}
					if !smp.hasFlags {
						flags = pick(traf.hasDefFlags, traf.defFlags, trex.flags)
					}
					end := off + uint64(size)
					if end > uint64(len(buffer)) {
						return nil, errors.New("video sample lies outside the file (truncated download?)")
					}
					samples = append(samples, mp4Sample{
						data: buffer[off:end],
						pts:  int64(dts) + smp.cto,
						sync: flags&sampleIsNonSync == 0,
					})
					dts += uint64(dur)
					off = end
				}
			}
			nextDTS = dts
		}
	}
	return samples, nil
}

func pick(has bool, value, fallback uint32) uint32 {
	if has {
		return value
	}
	return fallback
}

// editList returns the media time shown first, the delay of any leading
// empty edit (seconds), and the edited duration (seconds; 0 when unknown).
func editList(edits []mp4Edit, movieTimescale uint32) (shift int64, delay, duration float64) {
	if len(edits) == 0 || movieTimescale == 0 {
		return 0, 0, 0
	}
	movieScale := float64(movieTimescale)
	for _, e := range edits {
		if e.mediaTime < 0 {
			delay += float64(e.segmentDuration) / movieScale
			continue
		}
		return e.mediaTime, delay, delay + float64(e.segmentDuration)/movieScale
	}
	return 0, delay, 0
}

// trackRotation reads the display rotation from the track matrix, as players
// (and ffmpeg's autorotate) apply it. Phones record portrait video this way.
func trackRotation(a32, b32 int32) int {
	a, b := float64(a32), float64(b32)
	if a == 0 && b == 0 {
		return 0
	}
	deg := int(math.Round(math.Atan2(b, a)*180/math.Pi/90)) * 90
	return (deg%360 + 360) % 360
}

// ── decoded H.264 pictures ───────────────────────────────────────────────────

type yuvPicture struct{ f *goh264.Frame }

// planeGeometry locates plane idx (0 = Y): its samples, stride, the visible
// picture's offset within it, and its subsampling.
func (p *yuvPicture) planeGeometry(idx int) (data []byte, data16 []uint16, stride, offX, offY, sx, sy int) {
	f := p.f
	sx, sy = 1, 1
	if idx > 0 {
		switch f.ChromaFormatIDC {
		case 1:
			sx, sy = 2, 2
		case 2:
			sx = 2
		}
	}
	switch idx {
	case 0:
		return f.Y, f.Y16, f.YStride, f.CropLeft, f.CropTop, sx, sy
	case 1:
		return f.Cb, f.Cb16, f.CStride, f.CropLeft / sx, f.CropTop / sy, sx, sy
	default:
		return f.Cr, f.Cr16, f.CStride, f.CropLeft / sx, f.CropTop / sy, sx, sy
	}
}

// plane8 returns plane idx of the visible picture as 8-bit samples.
func (p *yuvPicture) plane8(idx int) plane {
	f := p.f
	data, data16, stride, offX, offY, sx, sy := p.planeGeometry(idx)
	w, h := (f.Width+sx-1)/sx, (f.Height+sy-1)/sy
	if data16 == nil {
		return plane{pix: data[offY*stride+offX:], w: w, h: h, stride: stride}
	}
	out := newPlane(w, h)
	depth := f.BitDepthLuma
	if idx > 0 {
		depth = f.BitDepthChroma
	}
	for y := range h {
		src := data16[(offY+y)*stride+offX:]
		dst := out.row(y)
		for x := range dst {
			dst[x] = byte(src[x] >> max(0, depth-8)) //nolint:gosec // shifted down to 8 bits
		}
	}
	return out
}

func (p *yuvPicture) planes(scale int) framePlanes {
	nPlanes := 3
	if p.f.ChromaFormatIDC == 0 {
		nPlanes = 1
	}
	out := make(framePlanes, 0, nPlanes)
	for i := range nPlanes {
		src := p.plane8(i)
		s := scale
		if i > 0 {
			// Chroma is already subsampled; shrink it to match luma's grid.
			_, _, _, _, _, sx, _ := p.planeGeometry(i)
			s = max(1, scale/sx)
		}
		out = append(out, boxDownscale(src, s))
	}
	return out
}

// image converts to RGB with the stream's matrix and range: BT.709 for HD
// and unspecified HD, BT.601 for SD, limited range unless flagged full.
func (p *yuvPicture) image() image.Image {
	f := p.f
	y, cb, cr := p.plane8(0), plane{}, plane{}
	mono := f.ChromaFormatIDC == 0
	if !mono {
		cb, cr = p.plane8(1), p.plane8(2)
	}
	kr, kb := 0.2126, 0.0722 // BT.709
	switch f.ColorMatrix {
	case 5, 6:
		kr, kb = 0.299, 0.114
	case 9, 10:
		kr, kb = 0.2627, 0.0593
	case 2: // unspecified: guess from size, as players do
		if f.Height < 720 {
			kr, kb = 0.299, 0.114
		}
	}
	full := f.VideoFullRangeFlag == 1
	yScale, yOff, cScale := 255.0/219.0, 16.0, 255.0/224.0
	if full {
		yScale, yOff, cScale = 1, 0, 1
	}
	kg := 1 - kr - kb
	const fix = 1 << 16
	ry := int(yScale * fix)
	rv := int(2 * (1 - kr) * cScale * fix)
	gu := int(2 * (1 - kb) * kb / kg * cScale * fix)
	gv := int(2 * (1 - kr) * kr / kg * cScale * fix)
	bu := int(2 * (1 - kb) * cScale * fix)
	yo := int(yOff)

	w, h := f.Width, f.Height
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	sx, sy := 1, 1
	if !mono {
		_, _, _, _, _, sx, sy = p.planeGeometry(1)
	}
	for row := range h {
		yrow := y.row(row)
		out := img.Pix[row*img.Stride : row*img.Stride+w*4]
		var cbRow, crRow []byte
		if !mono {
			cbRow, crRow = cb.row(row/sy), cr.row(row/sy)
		}
		for x := range w {
			lum := (int(yrow[x]) - yo) * ry
			r, g, b := lum, lum, lum
			if !mono {
				u := int(cbRow[x/sx]) - 128
				v := int(crRow[x/sx]) - 128
				r += rv * v
				g -= gu*u + gv*v
				b += bu * u
			}
			out[x*4] = clamp8(r >> 16)
			out[x*4+1] = clamp8(g >> 16)
			out[x*4+2] = clamp8(b >> 16)
			out[x*4+3] = 255
		}
	}
	return img
}

func clamp8(v int) byte {
	switch {
	case v < 0:
		return 0
	case v > 255:
		return 255
	}
	return byte(v)
}
