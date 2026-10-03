package main

import (
	"encoding/binary"
	"errors"
	"math"
)

// Matroska / WebM demuxing: just enough EBML to find the first video track,
// its codec, and its frames. Browser recorders (MediaRecorder) write these
// with unknown-size segments and clusters, which are handled by ending such
// an element where the next one of its level begins.

const (
	ebmlHeaderID   = 0x1A45DFA3
	mkvSegment     = 0x18538067
	mkvInfo        = 0x1549A966
	mkvTimecodeSc  = 0x2AD7B1
	mkvDuration    = 0x4489
	mkvTracks      = 0x1654AE6B
	mkvTrackEntry  = 0xAE
	mkvTrackNumber = 0xD7
	mkvTrackType   = 0x83
	mkvCodecID     = 0x86
	mkvCodecPriv   = 0x63A2
	mkvVideo       = 0xE0
	mkvPixelWidth  = 0xB0
	mkvPixelHeight = 0xBA
	mkvCluster     = 0x1F43B675
	mkvTimecode    = 0xE7
	mkvSimpleBlock = 0xA3
	mkvBlockGroup  = 0xA0
	mkvBlock       = 0xA1
	mkvRefBlock    = 0xFB
)

// mkvTopLevel are the Segment children; an unknown-size Cluster ends where
// one of them starts.
var mkvTopLevel = map[uint32]bool{
	0x114D9B74: true, mkvInfo: true, mkvTracks: true, mkvCluster: true,
	0x1C53BB6B: true, 0x1043A770: true, 0x1254C367: true, 0x1941A469: true,
}

var errTruncatedEBML = errors.New("truncated Matroska element")

// ebmlVint reads a variable-length integer. With keepMarker it returns the
// raw value (element IDs); otherwise the length marker is stripped and an
// all-ones value reports unknown (sizes).
func ebmlVint(b []byte, keepMarker bool) (v uint64, n int, unknown bool, err error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, errTruncatedEBML
	}
	n = 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false, errTruncatedEBML
	}
	v = uint64(b[0])
	if !keepMarker {
		v &= uint64(0xff >> n)
	}
	allOnes := v == uint64(0xff>>n)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
		allOnes = allOnes && c == 0xff
	}
	return v, n, !keepMarker && allOnes, nil
}

type ebmlElement struct {
	id      uint32
	data    []byte // the payload; for an unknown size, everything to the end of the parent
	unknown bool
	size    int // header + payload bytes consumed
}

func readEBML(b []byte) (ebmlElement, error) {
	id, n1, _, err := ebmlVint(b, true)
	if err != nil {
		return ebmlElement{}, err
	}
	size, n2, unknown, err := ebmlVint(b[n1:], false)
	if err != nil {
		return ebmlElement{}, err
	}
	head := n1 + n2
	if unknown {
		return ebmlElement{id: uint32(id), data: b[head:], unknown: true, size: len(b)}, nil //nolint:gosec // IDs are at most 4 bytes
	}
	if rest := uint64(len(b) - head); size > rest { //nolint:gosec // head <= len(b), so non-negative
		size = rest // a truncated download: keep what is there
	}
	return ebmlElement{id: uint32(id), data: b[head : head+int(size)], size: head + int(size)}, nil //nolint:gosec // IDs are at most 4 bytes; size bounded above
}

// eachEBML visits the children of a payload. A child of unknown size is cut
// where the next element for which ends(id) is true begins.
func eachEBML(b []byte, ends func(id uint32) bool, fn func(e ebmlElement) error) error {
	for len(b) > 0 {
		e, err := readEBML(b)
		if err != nil {
			return nil // trailing garbage or a cut-off file: stop quietly
		}
		if e.unknown {
			head := len(b) - len(e.data)
			e.data = cutUnknown(e.data, ends)
			e.size = head + len(e.data)
		}
		if err := fn(e); err != nil {
			return err
		}
		if e.size <= 0 {
			return nil
		}
		b = b[min(e.size, len(b)):]
	}
	return nil
}

// cutUnknown scans an unknown-size payload element by element and returns
// it up to the first element that belongs to an enclosing level.
func cutUnknown(b []byte, ends func(id uint32) bool) []byte {
	off := 0
	for off < len(b) {
		e, err := readEBML(b[off:])
		if err != nil || (ends != nil && ends(e.id)) {
			return b[:off]
		}
		off += e.size
	}
	return b
}

func ebmlUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func ebmlFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}

type mkvTrack struct {
	number        uint64
	codecID       string
	codecPrivate  []byte
	width, height int
}

func openMKVStream(buffer []byte) (*packetStream, error) {
	hdr, err := readEBML(buffer)
	if err != nil || hdr.id != ebmlHeaderID {
		return nil, errUnsupportedVideo
	}
	timecodeScale := 1_000_000.0 // ns per tick, Matroska's default
	duration := 0.0
	var track *mkvTrack
	var packets []videoPacket

	isTop := func(id uint32) bool { return mkvTopLevel[id] }
	err = eachEBML(buffer[hdr.size:], nil, func(seg ebmlElement) error {
		if seg.id != mkvSegment {
			return nil
		}
		return eachEBML(seg.data, isTop, func(e ebmlElement) error {
			switch e.id {
			case mkvInfo:
				_ = eachEBML(e.data, nil, func(c ebmlElement) error {
					switch c.id {
					case mkvTimecodeSc:
						if v := ebmlUint(c.data); v > 0 {
							timecodeScale = float64(v)
						}
					case mkvDuration:
						duration = ebmlFloat(c.data)
					}
					return nil
				})
			case mkvTracks:
				if track == nil {
					track = firstVideoTrack(e.data)
				}
			case mkvCluster:
				if track == nil {
					return nil
				}
				clusterBlocks(e.data, track.number, &packets)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if track == nil {
		return nil, errors.New("No video stream found.")
	}

	var codec videoCodec
	w, h := track.width, track.height
	switch track.codecID {
	case "V_MPEG4/ISO/AVC":
		var sw, sh int
		if codec, sw, sh, err = h264AVCC(track.codecPrivate); err != nil {
			return nil, err
		}
		if sw > 0 {
			w, h = sw, sh
		}
	case "V_MJPEG":
		codec = mjpegCodec
	case "V_VP8":
		codec = vp8Codec
	default:
		return nil, unsupportedCodec(track.codecID)
	}
	// Block timestamps are presentation times in ticks of timecodeScale ns.
	timing := streamTiming{timescale: 1e9 / timecodeScale, duration: duration * timecodeScale / 1e9}
	return newPacketStream(packets, codec, timing, w, h, 0)
}

func firstVideoTrack(tracks []byte) *mkvTrack {
	var found *mkvTrack
	_ = eachEBML(tracks, nil, func(e ebmlElement) error {
		if e.id != mkvTrackEntry || found != nil {
			return nil
		}
		t := &mkvTrack{}
		isVideo := false
		_ = eachEBML(e.data, nil, func(c ebmlElement) error {
			switch c.id {
			case mkvTrackNumber:
				t.number = ebmlUint(c.data)
			case mkvTrackType:
				isVideo = ebmlUint(c.data) == 1
			case mkvCodecID:
				t.codecID = string(c.data)
			case mkvCodecPriv:
				t.codecPrivate = c.data
			case mkvVideo:
				_ = eachEBML(c.data, nil, func(v ebmlElement) error {
					switch v.id {
					case mkvPixelWidth:
						t.width = int(ebmlUint(v.data)) //nolint:gosec // pixel sizes are small
					case mkvPixelHeight:
						t.height = int(ebmlUint(v.data)) //nolint:gosec // pixel sizes are small
					}
					return nil
				})
			}
			return nil
		})
		if isVideo {
			found = t
		}
		return nil
	})
	return found
}

// clusterBlocks appends the cluster's frames for track to packets.
func clusterBlocks(cluster []byte, track uint64, packets *[]videoPacket) {
	var base int64
	_ = eachEBML(cluster, nil, func(e ebmlElement) error {
		switch e.id {
		case mkvTimecode:
			base = int64(ebmlUint(e.data)) //nolint:gosec // timestamps fit in int64
		case mkvSimpleBlock:
			if p, ok := parseBlock(e.data, track, base); ok {
				p.sync = e.data[blockHeaderLen(e.data)-1]&0x80 != 0
				*packets = append(*packets, p)
			}
		case mkvBlockGroup:
			var p videoPacket
			ok, referenced := false, false
			_ = eachEBML(e.data, nil, func(c ebmlElement) error {
				switch c.id {
				case mkvBlock:
					p, ok = parseBlock(c.data, track, base)
				case mkvRefBlock:
					referenced = true
				}
				return nil
			})
			if ok {
				p.sync = !referenced
				*packets = append(*packets, p)
			}
		}
		return nil
	})
}

// blockHeaderLen is the length of a block's track number, timecode and flags.
func blockHeaderLen(b []byte) int {
	_, n, _, err := ebmlVint(b, false)
	if err != nil {
		return 0
	}
	return n + 3
}

// parseBlock reads a (Simple)Block for track. Laced blocks — several
// frames in one, which video tracks do not use in practice — are skipped.
func parseBlock(b []byte, track uint64, base int64) (videoPacket, bool) {
	num, n, _, err := ebmlVint(b, false)
	if err != nil || num != track || len(b) < n+3 {
		return videoPacket{}, false
	}
	rel := int16(binary.BigEndian.Uint16(b[n:])) //nolint:gosec // a signed 16-bit field
	if b[n+2]&0x06 != 0 {
		return videoPacket{}, false
	}
	return videoPacket{data: b[n+3:], pts: base + int64(rel)}, true
}
