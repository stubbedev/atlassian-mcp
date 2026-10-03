package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"math"
	"slices"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/disintegration/imaging"
	"github.com/thesyncim/goh264"
)

// mp4Stream decodes the H.264 video track of an MP4 or QuickTime file. Each
// GOP — a sync sample and the samples up to the next — is one segment, so
// GOPs decode in parallel on separate decoders.
type mp4Stream struct {
	meta     videoMeta
	avcC     []byte
	samples  []mp4Sample // decode order
	gops     [][2]int    // decode-order sample ranges [lo, hi)
	segs     [][2]int    // the same GOPs as display-index ranges
	display  []float64   // presentation time per display index
	rotation int         // clockwise degrees to rotate decoded frames for display
	px       int
}

type mp4Sample struct {
	data []byte
	pts  int64 // media timescale
	sync bool
}

func openMP4Stream(buffer []byte) (*mp4Stream, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(buffer), mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil || (f.Moov == nil && (f.Init == nil || f.Init.Moov == nil)) {
		return nil, errors.New("unsupported video format: only MP4/MOV files (H.264 video) and GIF/APNG/WebP animations can be sampled")
	}
	moov := f.Moov
	if moov == nil {
		moov = f.Init.Moov
	}
	var trak *mp4.TrakBox
	for _, t := range moov.Traks {
		if t.Mdia != nil && t.Mdia.Hdlr != nil && t.Mdia.Hdlr.HandlerType == "vide" {
			trak = t
			break
		}
	}
	if trak == nil {
		return nil, errors.New("No video stream found.")
	}
	stbl := trak.Mdia.Minf.Stbl
	if stbl.Stsd == nil || stbl.Stsd.AvcX == nil || stbl.Stsd.AvcX.AvcC == nil {
		return nil, fmt.Errorf("unsupported video codec %s: only H.264 can be decoded", sampleEntryName(stbl.Stsd))
	}
	var cfg bytes.Buffer
	if err := stbl.Stsd.AvcX.AvcC.Encode(&cfg); err != nil {
		return nil, err
	}
	s := &mp4Stream{avcC: cfg.Bytes()[8:]} // the record, without its box header
	timescale := float64(trak.Mdia.Mdhd.Timescale)
	if timescale <= 0 {
		return nil, errors.New("video track has no timescale")
	}

	if f.IsFragmented() {
		s.samples, err = fragmentedSamples(f, buffer, trak.Tkhd.TrackID, moov)
	} else {
		s.samples, err = progressiveSamples(stbl, buffer)
	}
	if err != nil {
		return nil, err
	}
	if len(s.samples) == 0 {
		return nil, errors.New("video track has no samples")
	}

	// GOPs in decode order; a stream that opens without a sync sample still
	// decodes from its first sample.
	start := 0
	for i := 1; i < len(s.samples); i++ {
		if s.samples[i].sync {
			s.gops = append(s.gops, [2]int{start, i})
			start = i
		}
	}
	s.gops = append(s.gops, [2]int{start, len(s.samples)})

	// The edit list's first media edit says which media time is shown at 0
	// and how long the movie is; an empty edit before it delays the start.
	shift, delay, editDuration := editList(trak, moov)
	for _, g := range s.gops {
		pts := make([]int64, 0, g[1]-g[0])
		for _, smp := range s.samples[g[0]:g[1]] {
			pts = append(pts, smp.pts)
		}
		slices.Sort(pts)
		lo := len(s.display)
		for _, p := range pts {
			t := float64(p-shift)/timescale + delay
			if p < shift {
				t = -1 // pre-roll: decoded, never shown
			}
			s.display = append(s.display, t)
		}
		s.segs = append(s.segs, [2]int{lo, len(s.display)})
	}

	duration := editDuration
	if duration <= 0 {
		duration = float64(trak.Mdia.Mdhd.Duration) / timescale
	}
	if duration <= 0 {
		last := s.display[len(s.display)-1]
		duration = last + last/float64(max(1, len(s.display)-1))
	}
	shown := 0
	for _, t := range s.display {
		if t >= 0 {
			shown++
		}
	}

	info, err := goh264.InspectAVCC(s.avcC)
	if err != nil {
		return nil, fmt.Errorf("invalid H.264 configuration: %w", err)
	}
	w, h := int(stbl.Stsd.AvcX.Width), int(stbl.Stsd.AvcX.Height)
	if info.StreamInfo.Width > 0 {
		w, h = info.StreamInfo.Width, info.StreamInfo.Height
	}
	s.px = max(1, w*h)
	s.rotation = trackRotation(trak.Tkhd)
	if s.rotation == 90 || s.rotation == 270 {
		w, h = h, w
	}
	s.meta = videoMeta{duration: duration, width: w, height: h, fps: float64(shown) / duration, codec: "h264"}
	return s, nil
}

func sampleEntryName(stsd *mp4.StsdBox) string {
	if stsd != nil && len(stsd.Children) > 0 {
		return stsd.Children[0].Type()
	}
	return "(unknown)"
}

// progressiveSamples walks a classic sample table: chunk offsets, samples
// per chunk, sizes, decode times, composition offsets and sync samples.
func progressiveSamples(stbl *mp4.StblBox, buffer []byte) ([]mp4Sample, error) {
	if stbl.Stsz == nil || stbl.Stsc == nil || stbl.Stts == nil {
		return nil, errors.New("video track is missing its sample table")
	}
	n := int(stbl.Stsz.SampleNumber)
	var offsets []uint64
	switch {
	case stbl.Stco != nil:
		for _, o := range stbl.Stco.ChunkOffset {
			offsets = append(offsets, uint64(o))
		}
	case stbl.Co64 != nil:
		offsets = stbl.Co64.ChunkOffset
	default:
		return nil, errors.New("video track has no chunk offsets")
	}
	var cts []int32
	if stbl.Ctts != nil {
		var err error
		if cts, err = stbl.Ctts.CompositionTimeOffsets(uint32(n)); err != nil { //nolint:gosec // sample counts are 32-bit in MP4
			return nil, err
		}
	}
	durs, err := stbl.Stts.SampleDurations(uint32(n)) //nolint:gosec // sample counts are 32-bit in MP4
	if err != nil {
		return nil, err
	}
	samples := make([]mp4Sample, 0, n)
	entries := stbl.Stsc.Entries
	var dts int64
	for e, entry := range entries {
		lastChunk := uint32(len(offsets)) //nolint:gosec // chunk counts are 32-bit in MP4
		if e+1 < len(entries) {
			lastChunk = entries[e+1].FirstChunk - 1
		}
		for chunk := entry.FirstChunk; chunk <= lastChunk && len(samples) < n; chunk++ {
			if int(chunk) > len(offsets) || chunk == 0 {
				return nil, errors.New("corrupt sample-to-chunk table")
			}
			off := offsets[chunk-1]
			for range entry.SamplesPerChunk {
				i := len(samples)
				if i >= n {
					break
				}
				size := uint64(stbl.Stsz.GetSampleSize(i + 1))
				if off+size > uint64(len(buffer)) {
					return nil, errors.New("video sample lies outside the file (truncated download?)")
				}
				pts := dts
				if cts != nil {
					pts += int64(cts[i])
				}
				sync := stbl.Stss == nil || stbl.Stss.IsSyncSample(uint32(i+1)) //nolint:gosec // sample counts are 32-bit in MP4
				samples = append(samples, mp4Sample{data: buffer[off : off+size], pts: pts, sync: sync})
				dts += int64(durs[i])
				off += size
			}
		}
	}
	return samples, nil
}

// fragmentedSamples collects a track's samples from every movie fragment,
// as written by recorders that stream (e.g. Safari's MediaRecorder).
func fragmentedSamples(f *mp4.File, buffer []byte, trackID uint32, moov *mp4.MoovBox) ([]mp4Sample, error) {
	var trex *mp4.TrexBox
	if moov.Mvex != nil {
		for _, t := range moov.Mvex.Trexs {
			if t.TrackID == trackID {
				trex = t
			}
		}
	}
	var samples []mp4Sample
	var nextDTS uint64
	for _, seg := range f.Segments {
		for _, frag := range seg.Fragments {
			if frag.Moof == nil {
				continue
			}
			for _, traf := range frag.Moof.Trafs {
				if traf.Tfhd == nil || traf.Tfhd.TrackID != trackID {
					continue
				}
				base := frag.Moof.StartPos
				if traf.Tfhd.HasBaseDataOffset() {
					base = traf.Tfhd.BaseDataOffset
				}
				dts := nextDTS
				if traf.Tfdt != nil {
					dts = traf.Tfdt.BaseMediaDecodeTime()
				}
				var off uint64
				for k, trun := range traf.Truns {
					trun.AddSampleDefaultValues(traf.Tfhd, trex)
					if trun.HasDataOffset() || k == 0 {
						off = uint64(int64(base) + int64(trun.DataOffset)) //nolint:gosec // offsets within the file
					}
					for _, smp := range trun.Samples {
						end := off + uint64(smp.Size)
						if end > uint64(len(buffer)) {
							return nil, errors.New("video sample lies outside the file (truncated download?)")
						}
						samples = append(samples, mp4Sample{
							data: buffer[off:end],
							pts:  int64(dts) + int64(smp.CompositionTimeOffset),
							sync: smp.IsSync(),
						})
						dts += uint64(smp.Dur)
						off = end
					}
				}
				nextDTS = dts
			}
		}
	}
	return samples, nil
}

// editList returns the media time shown first, the delay of any leading
// empty edit (seconds), and the edited duration (seconds; 0 when unknown).
func editList(trak *mp4.TrakBox, moov *mp4.MoovBox) (shift int64, delay, duration float64) {
	if trak.Edts == nil || len(trak.Edts.Elst) == 0 || moov.Mvhd == nil || moov.Mvhd.Timescale == 0 {
		return 0, 0, 0
	}
	movieScale := float64(moov.Mvhd.Timescale)
	for _, e := range trak.Edts.Elst[0].Entries {
		if e.MediaTime < 0 {
			delay += float64(e.SegmentDuration) / movieScale
			continue
		}
		return e.MediaTime, delay, delay + float64(e.SegmentDuration)/movieScale
	}
	return 0, delay, 0
}

// trackRotation reads the display rotation from the track matrix, as players
// (and ffmpeg's autorotate) apply it. Phones record portrait video this way.
func trackRotation(tkhd *mp4.TkhdBox) int {
	if tkhd == nil {
		return 0
	}
	a, b := float64(tkhd.Matrix[0]), float64(tkhd.Matrix[1])
	if a == 0 && b == 0 {
		return 0
	}
	deg := int(math.Round(math.Atan2(b, a)*180/math.Pi/90)) * 90
	return (deg%360 + 360) % 360
}

func (s *mp4Stream) info() videoMeta    { return s.meta }
func (s *mp4Stream) times() []float64   { return s.display }
func (s *mp4Stream) segments() [][2]int { return s.segs }
func (s *mp4Stream) pixels() int        { return s.px }

func (s *mp4Stream) decodeSegment(seg, last int, visit func(i int, p picture) bool) error {
	dec := goh264.NewDecoder()
	if _, err := dec.ConfigureAVCC(s.avcC); err != nil {
		return fmt.Errorf("invalid H.264 configuration: %w", err)
	}
	g := s.gops[seg]
	next := s.segs[seg][0]
	emit := func(frames []*goh264.Frame) bool {
		for _, fr := range frames {
			if next >= s.segs[seg][1] {
				return false
			}
			i := next
			next++
			if !visit(i, &yuvPicture{f: fr, rotation: s.rotation}) || i >= last {
				return false
			}
		}
		return true
	}
	for _, smp := range s.samples[g[0]:g[1]] {
		frames, err := dec.DecodeConfiguredAVCFrames(smp.data)
		if err != nil {
			return fmt.Errorf("H.264 decode failed: %w", err)
		}
		if !emit(frames) {
			return nil
		}
	}
	frames, err := dec.FlushDelayedFrames()
	if err != nil {
		return fmt.Errorf("H.264 decode failed: %w", err)
	}
	emit(frames)
	return nil
}

// ── decoded H.264 pictures ───────────────────────────────────────────────────

type yuvPicture struct {
	f        *goh264.Frame
	rotation int
}

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
	switch p.rotation {
	case 90:
		return imaging.Rotate270(img) // imaging rotates counter-clockwise
	case 180:
		return imaging.Rotate180(img)
	case 270:
		return imaging.Rotate90(img)
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
