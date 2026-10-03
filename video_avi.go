package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// AVI demuxing: the first video stream's frames from the movi list (and any
// OpenDML AVIX extensions), timed by the stream's constant frame rate.

func openAVIStream(buffer []byte) (*packetStream, error) {
	if len(buffer) < 12 || string(buffer[:4]) != "RIFF" || string(buffer[8:12]) != "AVI " {
		return nil, errUnsupportedVideo
	}
	var (
		streamIdx     = -1
		scale, rate   uint32
		fourcc        string
		width, height int
		frames        [][]byte // per frame slot; nil for a dropped frame
		index         []bool   // keyframe flags from idx1, in chunk order
	)
	chunkID := func() string { return fmt.Sprintf("%02d", streamIdx) }

	var collect func(list []byte)
	collect = func(list []byte) {
		_ = walkRIFF(list, func(id string, data []byte) error {
			if id == "LIST" && len(data) >= 4 && string(data[:4]) == "rec " {
				collect(data[4:])
				return nil
			}
			if streamIdx >= 0 && strings.HasPrefix(id, chunkID()) && (id[2:] == "dc" || id[2:] == "db") {
				frames = append(frames, data)
			}
			return nil
		})
	}

	// Top level: the AVI RIFF, then OpenDML's AVIX continuation RIFFs.
	err := walkRIFF(buffer, func(id string, data []byte) error {
		if id != "RIFF" || len(data) < 4 {
			return nil
		}
		return walkRIFF(data[4:], func(id string, data []byte) error {
			switch {
			case id == "LIST" && len(data) >= 4 && string(data[:4]) == "hdrl":
				streamIdx, scale, rate, fourcc, width, height = aviVideoStream(data[4:])
			case id == "LIST" && len(data) >= 4 && string(data[:4]) == "movi":
				collect(data[4:])
			case id == "idx1" && streamIdx >= 0:
				want := chunkID()
				for off := 0; off+16 <= len(data); off += 16 {
					ck := string(data[off : off+4])
					if strings.HasPrefix(ck, want) && (ck[2:] == "dc" || ck[2:] == "db") {
						index = append(index, binary.LittleEndian.Uint32(data[off+4:])&0x10 != 0)
					}
				}
			}
			return nil
		})
	})
	if err != nil && len(frames) == 0 {
		return nil, err
	}
	if streamIdx < 0 {
		return nil, errors.New("No video stream found.")
	}
	if scale == 0 || rate == 0 {
		return nil, errors.New("AVI video stream has no frame rate")
	}

	var codec videoCodec
	var packets []videoPacket
	switch strings.ToUpper(fourcc) {
	case "MJPG", "AVRN", "LJPG", "JPGL", "DMB1":
		codec = mjpegCodec
		for i, f := range frames {
			if len(f) > 0 {
				packets = append(packets, videoPacket{data: f, pts: int64(i), sync: true})
			}
		}
	case "H264", "X264", "AVC1", "DAVC":
		params := map[byte][]byte{}
		for i, f := range frames {
			if len(f) > 0 {
				idr := scanAnnexB(f, params)
				packets = append(packets, videoPacket{data: f, pts: int64(i), sync: idr || (i < len(index) && index[i])})
			}
		}
		var sw, sh int
		if codec, sw, sh, err = h264AnnexB(joinParamSets(params)); err != nil {
			return nil, err
		}
		if sw > 0 {
			width, height = sw, sh
		}
	default:
		return nil, unsupportedCodec(strings.TrimSpace(fourcc))
	}
	timing := streamTiming{timescale: float64(rate) / float64(scale), duration: float64(len(frames)) * float64(scale) / float64(rate)}
	return newPacketStream(packets, codec, timing, width, height, 0)
}

// aviVideoStream finds the first video stream in hdrl: its index, timing,
// codec fourcc and frame size.
func aviVideoStream(hdrl []byte) (idx int, scale, rate uint32, fourcc string, width, height int) {
	idx = -1
	n := 0
	_ = walkRIFF(hdrl, func(id string, data []byte) error {
		if idx >= 0 || id != "LIST" || len(data) < 4 || string(data[:4]) != "strl" {
			return nil
		}
		isVideo := false
		var sc, rt uint32
		_ = walkRIFF(data[4:], func(id string, d []byte) error {
			switch id {
			case "strh":
				if len(d) >= 28 && string(d[:4]) == "vids" {
					isVideo = true
					sc, rt = binary.LittleEndian.Uint32(d[20:]), binary.LittleEndian.Uint32(d[24:])
				}
			case "strf":
				if isVideo && len(d) >= 20 {
					width = int(int32(binary.LittleEndian.Uint32(d[4:])))  //nolint:gosec // BITMAPINFOHEADER sizes are signed 32-bit
					height = int(int32(binary.LittleEndian.Uint32(d[8:]))) //nolint:gosec // negative height means top-down
					height = max(height, -height)
					fourcc = string(d[16:20])
				}
			}
			return nil
		})
		if isVideo {
			idx, scale, rate = n, sc, rt
		}
		n++
		return nil
	})
	return idx, scale, rate, fourcc, width, height
}
