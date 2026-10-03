package main

import (
	"bytes"
	"errors"
	"image"
	"math"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/thesyncim/goh264"
)

// mp4Sample is a sample located in the file.
type mp4Sample = videoPacket

// openMP4Stream demuxes the video track of an MP4 or QuickTime file:
// H.264, or Motion JPEG as cameras and some screen tools write it.
func openMP4Stream(buffer []byte) (*packetStream, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(buffer), mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil || (f.Moov == nil && (f.Init == nil || f.Init.Moov == nil)) {
		return nil, errUnsupportedVideo
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
	if stbl.Stsd == nil {
		return nil, errors.New("video track has no sample description")
	}
	var codec videoCodec
	var w, h int
	switch entry := sampleEntryName(stbl.Stsd); {
	case stbl.Stsd.AvcX != nil && stbl.Stsd.AvcX.AvcC != nil:
		var cfg bytes.Buffer
		if err := stbl.Stsd.AvcX.AvcC.Encode(&cfg); err != nil {
			return nil, err
		}
		if codec, w, h, err = h264AVCC(cfg.Bytes()[8:]); err != nil { // the record, without its box header
			return nil, err
		}
		if w == 0 {
			w, h = int(stbl.Stsd.AvcX.Width), int(stbl.Stsd.AvcX.Height)
		}
	case entry == "jpeg" || entry == "mjpa":
		codec = mjpegCodec
		if vse, ok := stbl.Stsd.Children[0].(*mp4.VisualSampleEntryBox); ok {
			w, h = int(vse.Width), int(vse.Height)
		}
	default:
		return nil, unsupportedCodec(entry)
	}

	var packets []videoPacket
	if f.IsFragmented() {
		packets, err = fragmentedSamples(f, buffer, trak.Tkhd.TrackID, moov)
	} else {
		packets, err = progressiveSamples(stbl, buffer)
	}
	if err != nil {
		return nil, err
	}
	// The edit list's first media edit says which media time is shown at 0
	// and how long the movie is; an empty edit before it delays the start.
	shift, delay, duration := editList(trak, moov)
	timescale := float64(trak.Mdia.Mdhd.Timescale)
	if duration <= 0 && timescale > 0 {
		duration = float64(trak.Mdia.Mdhd.Duration) / timescale
	}
	timing := streamTiming{timescale: timescale, shift: shift, delay: delay, duration: duration}
	return newPacketStream(packets, codec, timing, w, h, trackRotation(trak.Tkhd))
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
