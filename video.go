package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultVideoFrames       = 6
	defaultVideoMaxDimension = 768
	defaultVideoQuality      = 65
	maxVideoSourceBytes      = 250 * 1024 * 1024
	videoFramesMin           = 1
	videoFramesMax           = 60
	defaultSceneThreshold    = 0.3
)

type videoMeta struct {
	duration float64
	width    int
	height   int
	fps      float64
	codec    string
}

type videoFrame struct {
	data         []byte
	timestampSec float64
	approximate  bool
}

type processVideoOpts struct {
	frames         int
	start          *float64
	end            *float64
	dedup          bool
	mode           string
	sceneThreshold float64
}

type processVideoResult struct {
	meta                  videoMeta
	frames                []videoFrame
	effectiveStart        float64
	effectiveEnd          float64
	dedupApplied          bool
	mode                  string
	approximateTimestamps bool
}

func ffmpegPath() string {
	if p := os.Getenv("ATLASSIAN_MCP_FFMPEG_PATH"); p != "" {
		return p
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

func ffprobePath() string {
	if p := os.Getenv("ATLASSIAN_MCP_FFPROBE_PATH"); p != "" {
		return p
	}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	return ""
}

func runCmd(cmd string, args ...string) (stdout []byte, stderr string, code int, err error) {
	c := exec.Command(cmd, args...)
	var outBuf, errBuf strings.Builder
	// capture stdout as bytes, stderr as string
	var stdoutBuf []byte
	pipeOut, _ := c.StdoutPipe()
	c.Stderr = &errBuf
	if err = c.Start(); err != nil {
		return nil, "", 0, err
	}
	if pipeOut != nil {
		stdoutBuf, _ = readAll(pipeOut)
	}
	werr := c.Wait()
	_ = outBuf
	stderr = errBuf.String()
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			return stdoutBuf, stderr, ee.ExitCode(), nil
		}
		return stdoutBuf, stderr, 0, werr
	}
	return stdoutBuf, stderr, 0, nil
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

// decodeStillViaFFmpeg decodes a single still frame from formats the pure-Go
// image decoders don't handle (AVIF, HEIC/HEIF, JPEG-XL, …) into PNG bytes.
// Used as a fallback when imaging.Decode fails. Returns an error if ffmpeg is
// unavailable or cannot decode the input.
func decodeStillViaFFmpeg(buffer []byte) ([]byte, error) {
	fm := ffmpegPath()
	if fm == "" {
		return nil, fmt.Errorf("ffmpeg unavailable")
	}
	dir, err := os.MkdirTemp(os.TempDir(), "atlmcp-img-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, buffer, 0o644); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "out.png")
	_, stderr, code, err := runCmd(fm, "-hide_banner", "-loglevel", "error", "-i", input, "-frames:v", "1", out)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "unknown error"
		}
		return nil, fmt.Errorf("ffmpeg decode failed: %s", msg)
	}
	return os.ReadFile(out)
}

func probeVideo(filePath string) (videoMeta, error) {
	fp := ffprobePath()
	if fp == "" {
		return videoMeta{}, fmt.Errorf("ffprobe binary unavailable. Set ATLASSIAN_MCP_FFPROBE_PATH or install ffprobe.")
	}
	stdout, stderr, code, err := runCmd(fp,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,r_frame_rate,codec_name,duration,nb_frames,nb_read_frames:format=duration",
		"-count_frames",
		"-of", "json",
		filePath,
	)
	if err != nil {
		return videoMeta{}, err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "unknown error"
		}
		return videoMeta{}, fmt.Errorf("ffprobe failed (%d): %s", code, msg)
	}
	var data struct {
		Streams []struct {
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			RFrameRate   string `json:"r_frame_rate"`
			CodecName    string `json:"codec_name"`
			Duration     string `json:"duration"`
			NbFrames     string `json:"nb_frames"`
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(stdout, &data); err != nil {
		return videoMeta{}, err
	}
	if len(data.Streams) == 0 {
		return videoMeta{}, fmt.Errorf("No video stream found.")
	}
	s := data.Streams[0]
	rate := s.RFrameRate
	if rate == "" {
		rate = "0/1"
	}
	fps := 0.0
	if parts := strings.SplitN(rate, "/", 2); len(parts) == 2 {
		num, e1 := strconv.ParseFloat(parts[0], 64)
		den, e2 := strconv.ParseFloat(parts[1], 64)
		if e1 == nil && e2 == nil && den > 0 {
			fps = num / den
		}
	}
	formatDuration, _ := strconv.ParseFloat(data.Format.Duration, 64)
	streamDuration, _ := strconv.ParseFloat(s.Duration, 64)
	frameCount := 0
	if s.NbFrames != "" {
		frameCount, _ = strconv.Atoi(s.NbFrames)
	} else if s.NbReadFrames != "" {
		frameCount, _ = strconv.Atoi(s.NbReadFrames)
	}
	fromFrames := 0.0
	if frameCount > 0 && fps > 0 {
		fromFrames = float64(frameCount) / fps
	}
	duration := 0.0
	for _, v := range []float64{formatDuration, streamDuration, fromFrames} {
		if v > 0 {
			duration = v
			break
		}
	}
	codec := s.CodecName
	if codec == "" {
		codec = "unknown"
	}
	return videoMeta{duration: duration, width: s.Width, height: s.Height, fps: fps, codec: codec}, nil
}

func quickHash(buf []byte) string {
	h := sha1.New()
	const head = 1024 * 1024
	end := head
	if len(buf) < end {
		end = len(buf)
	}
	h.Write(buf[:end])
	if len(buf) > head*2 {
		h.Write(buf[len(buf)-head:])
	}
	h.Write([]byte(strconv.Itoa(len(buf))))
	return hex.EncodeToString(h.Sum(nil))
}

const videoCacheMax = 16

var (
	videoCache      = map[string]*processVideoResult{}
	videoCacheOrder []string
)

func videoCacheGet(key string) *processVideoResult {
	hit, ok := videoCache[key]
	if !ok {
		return nil
	}
	// refresh LRU order
	for i, k := range videoCacheOrder {
		if k == key {
			videoCacheOrder = append(videoCacheOrder[:i], videoCacheOrder[i+1:]...)
			break
		}
	}
	videoCacheOrder = append(videoCacheOrder, key)
	return hit
}

func videoCacheSet(key string, value *processVideoResult) {
	if len(videoCache) >= videoCacheMax && len(videoCacheOrder) > 0 {
		oldest := videoCacheOrder[0]
		videoCacheOrder = videoCacheOrder[1:]
		delete(videoCache, oldest)
	}
	videoCache[key] = value
	videoCacheOrder = append(videoCacheOrder, key)
}

var showinfoTsRe = regexp.MustCompile(`Parsed_showinfo[^\]]*\][^\n]*pts_time:([\d.]+)`)

func parseShowinfoTimestamps(stderr string) []float64 {
	var out []float64
	for _, m := range showinfoTsRe.FindAllStringSubmatch(stderr, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func processVideo(buffer []byte, opts processVideoOpts) (*processVideoResult, error) {
	fm := ffmpegPath()
	if fm == "" {
		return nil, fmt.Errorf("ffmpeg binary unavailable. Set ATLASSIAN_MCP_FFMPEG_PATH or install ffmpeg.")
	}
	frames := opts.frames
	if frames < videoFramesMin {
		frames = videoFramesMin
	} else if frames > videoFramesMax {
		frames = videoFramesMax
	}
	dedup := opts.dedup
	mode := opts.mode
	if mode == "" {
		mode = "uniform"
	}
	sceneThreshold := opts.sceneThreshold
	if sceneThreshold < 0.01 {
		sceneThreshold = 0.01
	} else if sceneThreshold > 1 {
		sceneThreshold = 1
	}

	startKey := "a"
	if opts.start != nil {
		startKey = strconv.FormatFloat(*opts.start, 'f', -1, 64)
	}
	endKey := "z"
	if opts.end != nil {
		endKey = strconv.FormatFloat(*opts.end, 'f', -1, 64)
	}
	cacheKey := fmt.Sprintf("%s:%d:%s:%s:%t:%s:%g", quickHash(buffer), frames, startKey, endKey, dedup, mode, sceneThreshold)
	if cached := videoCacheGet(cacheKey); cached != nil {
		return cached, nil
	}

	dir, err := os.MkdirTemp(os.TempDir(), "atlmcp-video-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, buffer, 0o644); err != nil {
		return nil, err
	}
	meta, err := probeVideo(input)
	if err != nil {
		return nil, err
	}

	start := 0.0
	if opts.start != nil && *opts.start > 0 {
		start = *opts.start
	}
	endRaw := meta.duration
	if opts.end != nil {
		endRaw = *opts.end
	}
	end := endRaw
	if meta.duration > 0 && meta.duration < end {
		end = meta.duration
	}
	if end <= start {
		return nil, fmt.Errorf("Invalid window: start=%gs end=%gs (duration=%gs).", start, end, meta.duration)
	}
	window := end - start

	extractWithMode := func(curMode string, useDedup bool) ([]videoFrame, bool, error) {
		var vfParts []string
		if curMode == "scenes" {
			vfParts = append(vfParts, fmt.Sprintf(`select='gt(scene\,%.3f)'`, sceneThreshold))
		} else {
			fps := float64(frames) / window
			if fps < 0.001 {
				fps = 0.001
			}
			vfParts = append(vfParts, fmt.Sprintf("fps=%g", fps))
		}
		if useDedup {
			vfParts = append(vfParts, "mpdecimate=hi=64*12:lo=64*5:frac=0.33")
		}
		vfParts = append(vfParts, "showinfo")
		vf := strings.Join(vfParts, ",")
		_, stderr, code, err := runCmd(fm,
			"-hide_banner", "-loglevel", "info",
			"-ss", strconv.FormatFloat(start, 'f', 3, 64),
			"-to", strconv.FormatFloat(end, 'f', 3, 64),
			"-i", input,
			"-vf", vf,
			"-frames:v", strconv.Itoa(frames),
			"-fps_mode", "vfr",
			"-an", "-sn",
			"-q:v", "2",
			filepath.Join(dir, "frame-%03d.jpg"),
		)
		if err != nil {
			return nil, false, err
		}
		if code != 0 {
			var errLines []string
			for _, l := range strings.Split(stderr, "\n") {
				if strings.Contains(strings.ToLower(l), "error") {
					errLines = append(errLines, l)
				}
			}
			if len(errLines) > 3 {
				errLines = errLines[len(errLines)-3:]
			}
			msg := strings.TrimSpace(strings.Join(errLines, " / "))
			if msg == "" {
				msg = "unknown error"
			}
			return nil, false, fmt.Errorf("ffmpeg failed (%d): %s", code, msg)
		}
		entries, _ := os.ReadDir(dir)
		var files []string
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "frame-") && strings.HasSuffix(n, ".jpg") {
				files = append(files, n)
			}
		}
		sort.Strings(files)
		ptsTimes := parseShowinfoTimestamps(stderr)
		exact := len(ptsTimes) == len(files)
		var out []videoFrame
		step := window / float64(max(1, len(files)))
		for i, f := range files {
			data, _ := os.ReadFile(filepath.Join(dir, f))
			ts := start + step*(float64(i)+0.5)
			if exact {
				ts = start + ptsTimes[i]
			}
			out = append(out, videoFrame{data: data, timestampSec: ts, approximate: !exact})
		}
		return out, !exact, nil
	}

	cleanFrames := func() {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "frame-") {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	extracted, approximate, err := extractWithMode(mode, dedup)
	if err != nil {
		return nil, err
	}
	dedupApplied := dedup
	effectiveMode := mode

	if len(extracted) == 0 && mode == "scenes" {
		cleanFrames()
		effectiveMode = "uniform"
		extracted, approximate, err = extractWithMode("uniform", dedup)
		if err != nil {
			return nil, err
		}
	}
	if len(extracted) == 0 && dedup {
		cleanFrames()
		extracted, approximate, err = extractWithMode(effectiveMode, false)
		if err != nil {
			return nil, err
		}
		dedupApplied = false
	}

	result := &processVideoResult{
		meta:                  meta,
		frames:                extracted,
		effectiveStart:        start,
		effectiveEnd:          end,
		dedupApplied:          dedupApplied,
		mode:                  effectiveMode,
		approximateTimestamps: approximate,
	}
	videoCacheSet(cacheKey, result)
	return result, nil
}
