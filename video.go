package main

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // SHA-1 is used only as a change-detection digest, not for security
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"runtime"
	"sort"
	"strconv"
	"sync"
)

// Video and animated-image frame sampling, in-process: MP4/MOV with H.264
// (mp4ff demuxing, goh264 decoding) and GIF/APNG/animated WebP. Frames are
// sampled uniformly or at scene changes, and near-duplicates are dropped the
// way ffmpeg's mpdecimate does.

const (
	defaultVideoFrames       = 6
	defaultVideoMaxDimension = 768
	defaultVideoQuality      = 65
	maxVideoSourceBytes      = 250 * 1024 * 1024
	videoFramesMin           = 1
	videoFramesMax           = 60
	defaultSceneThreshold    = 0.3

	// Frames are cached and handed on as JPEG at this quality; the caller
	// re-encodes at the requested size and quality.
	cachedFrameQuality = 92

	// decodeBudget caps the estimated decode work for one request. Past it,
	// sampling falls back to keyframes, which decode on their own.
	decodeBudget = 30.0 // seconds
	// decodeCostPerPixel is goh264's measured cost per decoded pixel, one core.
	decodeCostPerPixel = 8e-9 // seconds
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
	note                  string
}

// frameStream is a decodable sequence of frames in display order, split into
// segments that decode independently of each other (an H.264 GOP; a whole
// animation).
type frameStream interface {
	info() videoMeta
	// times is each frame's presentation time in seconds, ascending. A
	// negative time marks a frame decoded only as a reference (an edit list's
	// pre-roll) and never shown.
	times() []float64
	// segments are the half-open display-index ranges [lo, hi) that decode
	// independently, in order, covering every frame.
	segments() [][2]int
	// decodeSegment decodes segment seg in order, calling visit for each frame
	// up to display index last (inclusive); visit returning false stops early.
	decodeSegment(seg, last int, visit func(i int, p picture) bool) error
	// pixels is the decode cost of one frame, for budgeting.
	pixels() int
}

// picture is one decoded frame: an image, and a cheap way to get its
// downscaled planes for comparisons without converting it to RGB first.
type picture interface {
	image() image.Image
	planes(scale int) framePlanes
}

// plane is one 8-bit image plane; rows are stride bytes apart.
type plane struct {
	pix          []byte
	w, h, stride int
}

func newPlane(w, h int) plane { return plane{pix: make([]byte, w*h), w: w, h: h, stride: w} }

func (p plane) row(y int) []byte { return p.pix[y*p.stride : y*p.stride+p.w] }

// boxDownscale averages s×s blocks into a new plane; edge pixels that do not
// fill a block are dropped.
func boxDownscale(src plane, s int) plane {
	if s < 1 || src.w < s || src.h < s {
		s = 1
	}
	w, h := src.w/s, src.h/s
	out := newPlane(w, h)
	area := uint32(s * s) //nolint:gosec // s is a small scale factor
	acc := make([]uint32, w)
	for y := range h {
		clear(acc)
		for dy := range s {
			row := src.row(y*s + dy)[:w*s]
			switch s { // unrolled for the scales sampling uses
			case 2:
				for x := range acc {
					r := row[x*2 : x*2+2]
					acc[x] += uint32(r[0]) + uint32(r[1])
				}
			case 4:
				for x := range acc {
					r := row[x*4 : x*4+4]
					acc[x] += uint32(r[0]) + uint32(r[1]) + uint32(r[2]) + uint32(r[3])
				}
			default:
				for x := range acc {
					for _, v := range row[x*s : x*s+s] {
						acc[x] += uint32(v)
					}
				}
			}
		}
		dst := out.row(y)
		for x, v := range acc {
			dst[x] = byte(v / area) //nolint:gosec // an average of bytes fits a byte
		}
	}
	return out
}

// framePlanes is a frame downscaled by box averaging: luma, then any chroma.
type framePlanes []plane

// quickHash fingerprints a buffer cheaply for the frame cache key.
func quickHash(buf []byte) string {
	h := sha1.New() //nolint:gosec // digest for change detection, not security
	const head = 1024 * 1024
	end := min(len(buf), head)
	h.Write(buf[:end])
	if len(buf) > head*2 {
		h.Write(buf[len(buf)-head:])
	}
	h.Write([]byte(strconv.Itoa(len(buf))))
	return hex.EncodeToString(h.Sum(nil))
}

const videoCacheMax = 16

var (
	videoCacheMu    sync.Mutex
	videoCache      = map[string]*processVideoResult{}
	videoCacheOrder []string
)

func videoCacheGet(key string) *processVideoResult {
	videoCacheMu.Lock()
	defer videoCacheMu.Unlock()
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
	videoCacheMu.Lock()
	defer videoCacheMu.Unlock()
	if len(videoCache) >= videoCacheMax && len(videoCacheOrder) > 0 {
		oldest := videoCacheOrder[0]
		videoCacheOrder = videoCacheOrder[1:]
		delete(videoCache, oldest)
	}
	videoCache[key] = value
	videoCacheOrder = append(videoCacheOrder, key)
}

var errUnsupportedVideo = errors.New("unsupported video format: MP4/MOV, Matroska/WebM and AVI files and GIF/APNG/WebP animations can be sampled")

func unsupportedCodec(name string) error {
	return fmt.Errorf("unsupported video codec %s: H.264, Motion JPEG and VP8 (keyframes) can be decoded", orValue(name, "(unknown)"))
}

// openFrameStream picks a demuxer by content: an animated image, Matroska
// or WebM, AVI, else MP4/QuickTime.
func openFrameStream(buffer []byte) (frameStream, error) {
	if s, ok, err := openAnimation(buffer); ok {
		return s, err
	}
	switch {
	case bytes.HasPrefix(buffer, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return openMKVStream(buffer)
	case len(buffer) >= 12 && string(buffer[:4]) == "RIFF" && string(buffer[8:12]) == "AVI ":
		return openAVIStream(buffer)
	}
	return openMP4Stream(buffer)
}

// processVideo samples frames from a video or animation. A malformed file
// can make a demuxer or decoder panic; that is reported as an error rather
// than taking the server down.
func processVideo(buffer []byte, opts processVideoOpts) (res *processVideoResult, err error) {
	err = recoverDecode(func() error {
		res, err = sampleVideo(buffer, opts)
		return err
	})
	return res, err
}

// recoverDecode runs fn, turning a panic into an error.
func recoverDecode(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the file could not be decoded (it may be corrupt or truncated): %v", r)
		}
	}()
	return fn()
}

func sampleVideo(buffer []byte, opts processVideoOpts) (*processVideoResult, error) {
	frames := min(max(opts.frames, videoFramesMin), videoFramesMax)
	mode := opts.mode
	if mode == "" {
		mode = "uniform"
	}
	sceneThreshold := min(max(opts.sceneThreshold, 0.01), 1)

	startKey := "a"
	if opts.start != nil {
		startKey = strconv.FormatFloat(*opts.start, 'f', -1, 64)
	}
	endKey := "z"
	if opts.end != nil {
		endKey = strconv.FormatFloat(*opts.end, 'f', -1, 64)
	}
	cacheKey := fmt.Sprintf("%s:%d:%s:%s:%t:%s:%g", quickHash(buffer), frames, startKey, endKey, opts.dedup, mode, sceneThreshold)
	if cached := videoCacheGet(cacheKey); cached != nil {
		return cached, nil
	}

	stream, err := openFrameStream(buffer)
	if err != nil {
		return nil, err
	}
	meta := stream.info()

	start := 0.0
	if opts.start != nil && *opts.start > 0 {
		start = *opts.start
	}
	end := meta.duration
	if opts.end != nil && *opts.end < end {
		end = *opts.end
	}
	if end <= start {
		return nil, fmt.Errorf("Invalid window: start=%gs end=%gs (duration=%gs).", start, end, meta.duration)
	}
	times := stream.times()
	lo := sort.SearchFloat64s(times, start)
	hi := sort.Search(len(times), func(i int) bool { return times[i] > end })
	if lo >= hi {
		// A window narrower than one frame still shows the frame on screen then.
		lo = max(0, lo-1)
		for lo < len(times)-1 && times[lo] < 0 {
			lo++
		}
		hi = lo + 1
	}

	s := &sampler{stream: stream, times: times, lo: lo, hi: hi}
	picks, note := s.uniformPicks(start, end, frames)
	streamNote := ""
	if n, ok := stream.(interface{ note() string }); ok {
		// Says why sampling is keyframe-bound, which covers the budget note.
		streamNote = n.note()
	}
	effectiveMode := "uniform"
	have := map[int]renderedFrame{}
	if mode == "scenes" {
		// One pass scores every frame and renders the scene changes it finds,
		// plus the uniform picks in case there are none.
		scenes, sceneNote, scanned, err := s.scenePicks(sceneThreshold, frames*4, picks)
		if err != nil {
			return nil, err
		}
		have = scanned
		if len(scenes) > 0 {
			picks, note, effectiveMode = scenes, sceneNote, "scenes"
		}
	}

	rendered, err := s.render(picks, have)
	if err != nil {
		return nil, err
	}
	dedupApplied := opts.dedup
	kept := rendered
	if opts.dedup {
		kept = dropNearDuplicates(rendered)
	}
	if len(kept) > frames {
		kept = kept[:frames]
	}
	out := make([]videoFrame, len(kept))
	for i, r := range kept {
		out[i] = videoFrame{data: r.jpeg, timestampSec: times[r.index]}
	}

	result := &processVideoResult{
		meta:           meta,
		frames:         out,
		effectiveStart: start,
		effectiveEnd:   end,
		dedupApplied:   dedupApplied,
		mode:           effectiveMode,
		note:           orValue(streamNote, note),
	}
	videoCacheSet(cacheKey, result)
	return result, nil
}

type sampler struct {
	stream frameStream
	times  []float64
	lo, hi int // display-index window [lo, hi)
}

// segmentOf returns the index of the segment holding frame i.
func segmentOf(segs [][2]int, i int) int {
	return sort.Search(len(segs), func(s int) bool { return segs[s][1] > i })
}

// decodeSeconds estimates the wall time to decode, in parallel across
// segments, each segment from its start up to the given last frame.
func (s *sampler) decodeSeconds(lastBySeg map[int]int) float64 {
	segs := s.stream.segments()
	total, longest := 0.0, 0.0
	for seg, last := range lastBySeg {
		n := float64(last-segs[seg][0]+1) * float64(s.stream.pixels()) * decodeCostPerPixel
		total += n
		longest = math.Max(longest, n)
	}
	return math.Max(longest, total/float64(runtime.NumCPU()))
}

// uniformPicks spreads n frames evenly over [start, end], like ffmpeg's fps
// filter: each at the middle of its slice of the window. When decoding them
// would blow the budget (long GOPs), it settles for the nearest keyframes.
func (s *sampler) uniformPicks(start, end float64, n int) ([]int, string) {
	window := end - start
	picks := make([]int, 0, n)
	for i := range n {
		t := start + window*(float64(i)+0.5)/float64(n)
		picks = append(picks, s.nearest(t, s.lo, s.hi))
	}
	picks = uniqueSorted(picks)

	segs := s.stream.segments()
	need := map[int]int{}
	for _, p := range picks {
		seg := segmentOf(segs, p)
		need[seg] = max(need[seg], p)
	}
	if s.decodeSeconds(need) <= decodeBudget {
		return picks, ""
	}
	var keys []int
	for _, seg := range segs {
		if k := seg[0]; k >= s.lo && k < s.hi && s.times[k] >= 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return picks, ""
	}
	snapped := make([]int, 0, len(picks))
	for _, p := range picks {
		k := sort.SearchInts(keys, p)
		switch {
		case k == len(keys):
			k--
		case k > 0 && p-keys[k-1] < keys[k]-p:
			k--
		}
		snapped = append(snapped, keys[k])
	}
	return uniqueSorted(snapped), "Keyframes are far apart in this video, so frames were taken at the nearest keyframes to stay fast; narrow start/end for exact positions."
}

// nearest finds the shown frame in [lo, hi) whose time is closest to t.
func (s *sampler) nearest(t float64, lo, hi int) int {
	i := lo + sort.SearchFloat64s(s.times[lo:hi], t)
	switch {
	case i >= hi:
		return hi - 1
	case i > lo && t-s.times[i-1] <= s.times[i]-t:
		return i - 1
	}
	return i
}

func uniqueSorted(xs []int) []int {
	sort.Ints(xs)
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// sceneScaleDiv downsizes frames before scene scoring: scene changes are
// large, so a box-averaged frame scores them the same at a fraction of the cost.
const sceneScaleDiv = 4

// scenePicks returns the frames in the window whose scene score — ffmpeg's
// select filter metric — exceeds threshold, at most limit of them. Past the
// decode budget it scores keyframes against each other instead, since
// encoders place keyframes at scene cuts.
//
// The scan renders what it can while the frames are at hand: every frame in
// also, and each scene change whose score is known within its segment. A
// segment's first two frames are scored only once the previous segment is
// in, so those are left for render to fetch if picked.
func (s *sampler) scenePicks(threshold float64, limit int, also []int) ([]int, string, map[int]renderedFrame, error) {
	segs := s.stream.segments()
	first, last := segmentOf(segs, s.lo), segmentOf(segs, s.hi-1)
	need := map[int]int{}
	for seg := first; seg <= last; seg++ {
		need[seg] = min(segs[seg][1], s.hi) - 1
	}
	keyframesOnly := s.decodeSeconds(need) > decodeBudget
	wantAlso := map[int]bool{}
	for _, i := range also {
		wantAlso[i] = true
	}

	type segResult struct {
		idx      []int
		mafd     []float64 // vs. the previous frame in this segment; NaN for the first
		firstPic framePlanes
		lastPic  framePlanes
		rendered []renderedFrame
		err      error
	}
	results := make([]segResult, last-first+1)
	err := parallelDo(last-first+1, func(k int) error {
		seg := first + k
		r := &results[k]
		stop := need[seg]
		if keyframesOnly {
			stop = segs[seg][0]
		}
		var prev framePlanes
		prevMafd, scored := 0.0, 0
		err := s.stream.decodeSegment(seg, stop, func(i int, p picture) bool {
			if keyframesOnly && i != segs[seg][0] {
				return i < stop
			}
			if i < s.lo || s.times[i] < 0 {
				// Before the window, but the frame still anchors the first score.
				prev = p.planes(sceneScaleDiv)
				return true
			}
			cur := p.planes(sceneScaleDiv)
			r.idx = append(r.idx, i)
			mafd := math.NaN()
			if prev == nil {
				r.firstPic = cur
			} else {
				mafd = meanAbsDiff(prev, cur)
			}
			r.mafd = append(r.mafd, mafd)
			hit := false
			if !math.IsNaN(mafd) {
				scored++
				hit = scored >= 2 && sceneScore(mafd, prevMafd) > threshold
				prevMafd = mafd
			}
			if wantAlso[i] || (hit && len(r.rendered) < limit) {
				f, err := renderPicture(i, p)
				if err != nil {
					r.err = err
					return false
				}
				r.rendered = append(r.rendered, f)
			}
			prev = cur
			r.lastPic = cur
			return true
		})
		if err == nil {
			err = r.err
		}
		return err
	})
	if err != nil {
		return nil, "", nil, err
	}

	var picks []int
	rendered := map[int]renderedFrame{}
	var prevPlanes framePlanes
	prevMafd := 0.0
	for _, r := range results {
		for _, f := range r.rendered {
			rendered[f.index] = f
		}
		for j, i := range r.idx {
			mafd := r.mafd[j]
			if math.IsNaN(mafd) {
				if prevPlanes == nil {
					continue // the first frame has nothing to differ from
				}
				mafd = meanAbsDiff(prevPlanes, r.firstPic)
			}
			score := sceneScore(mafd, prevMafd)
			prevMafd = mafd
			if score > threshold && len(picks) < limit {
				picks = append(picks, i)
			}
		}
		if r.lastPic != nil {
			prevPlanes = r.lastPic
		}
	}
	note := ""
	if keyframesOnly {
		note = "This window is too long to scan every frame, so scene changes were looked for between keyframes only; narrow start/end for a full scan."
	}
	return picks, note, rendered, nil
}

// sceneScore is ffmpeg's: a frame's mean absolute difference from the one
// before, damped by how much that differs from the previous frame's, so a
// steady pan scores low and a cut scores high.
func sceneScore(mafd, prevMafd float64) float64 {
	return min(max(math.Min(mafd, math.Abs(mafd-prevMafd))/100, 0), 1)
}

func meanAbsDiff(a, b framePlanes) float64 {
	var sum, n int
	for p := range a {
		if p >= len(b) || a[p].w != b[p].w || a[p].h != b[p].h {
			return 0
		}
		for y := range a[p].h {
			ra, rb := a[p].row(y), b[p].row(y)
			for x, v := range ra {
				d := int(v) - int(rb[x])
				if d < 0 {
					d = -d
				}
				sum += d
			}
		}
		n += a[p].w * a[p].h
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

type renderedFrame struct {
	index int
	jpeg  []byte
	luma  plane // half-size luma for duplicate detection
}

func renderPicture(i int, p picture) (renderedFrame, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, p.image(), &jpeg.Options{Quality: cachedFrameQuality}); err != nil {
		return renderedFrame{}, err
	}
	return renderedFrame{index: i, jpeg: buf.Bytes(), luma: p.planes(2)[0]}, nil
}

// render returns the picked frames (sorted display indices) as JPEG, taking
// those in have as they are and decoding each segment the rest need once,
// in parallel.
func (s *sampler) render(picks []int, have map[int]renderedFrame) ([]renderedFrame, error) {
	segs := s.stream.segments()
	bySeg := map[int][]int{}
	var order []int
	var out []renderedFrame
	for _, p := range picks {
		if f, ok := have[p]; ok {
			out = append(out, f)
			continue
		}
		seg := segmentOf(segs, p)
		if _, ok := bySeg[seg]; !ok {
			order = append(order, seg)
		}
		bySeg[seg] = append(bySeg[seg], p)
	}
	results := make([][]renderedFrame, len(order))
	err := parallelDo(len(order), func(k int) error {
		seg := order[k]
		want := bySeg[seg]
		next := 0
		var renderErr error
		err := s.stream.decodeSegment(seg, want[len(want)-1], func(i int, p picture) bool {
			if i != want[next] {
				return true
			}
			f, err := renderPicture(i, p)
			if err != nil {
				renderErr = err
				return false
			}
			results[k] = append(results[k], f)
			next++
			return next < len(want)
		})
		if err == nil {
			err = renderErr
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, r := range results {
		out = append(out, r...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	if len(out) == 0 && len(picks) > 0 {
		return nil, errors.New("the decoder produced no frames")
	}
	return out, nil
}

// mpdecimate's defaults, per 8×8 block of full-size luma: a block differs
// when its summed absolute difference passes hi; a frame is a duplicate of
// the last kept one when no block passes hi and under frac of them pass lo.
// Frames are compared at half size, so a block here is 4×4.
const (
	decimateBlock = 4
	decimateHi    = 12 * decimateBlock * decimateBlock
	decimateLo    = 5 * decimateBlock * decimateBlock
	decimateFrac  = 0.33
)

func dropNearDuplicates(frames []renderedFrame) []renderedFrame {
	var kept []renderedFrame
	for _, f := range frames {
		if len(kept) > 0 && isNearDuplicate(kept[len(kept)-1].luma, f.luma) {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

func isNearDuplicate(a, b plane) bool {
	if a.w != b.w || a.h != b.h || a.w == 0 {
		return false
	}
	blocks, overLo := 0, 0
	for by := 0; by+decimateBlock <= a.h; by += decimateBlock {
		for bx := 0; bx+decimateBlock <= a.w; bx += decimateBlock {
			sad := 0
			for y := by; y < by+decimateBlock; y++ {
				ra, rb := a.row(y)[bx:bx+decimateBlock], b.row(y)[bx:bx+decimateBlock]
				for x, v := range ra {
					d := int(v) - int(rb[x])
					if d < 0 {
						d = -d
					}
					sad += d
				}
			}
			if sad > decimateHi {
				return false
			}
			if sad > decimateLo {
				overLo++
			}
			blocks++
		}
	}
	return blocks > 0 && float64(overLo) < decimateFrac*float64(blocks)
}

// parallelDo runs fn for 0..n-1 on up to NumCPU goroutines, returning the
// first error.
func parallelDo(n int, fn func(k int) error) error {
	workers := min(n, runtime.NumCPU())
	jobs := make(chan int)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for k := range jobs {
				if err := recoverDecode(func() error { return fn(k) }); err != nil {
					errs <- err
				}
			}
		})
	}
	for k := range n {
		jobs <- k
	}
	close(jobs)
	wg.Wait()
	close(errs)
	return <-errs
}
