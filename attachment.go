package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/ledongthuc/pdf"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	maxInlineBytes      = 10 * 1024 * 1024
	defaultMaxDimension = 1568
	defaultJpegQuality  = 85
)

func formatBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
}

func isTextMime(mimeType string) bool {
	mt := strings.ToLower(mimeType)
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	for _, m := range []string{
		"application/json", "application/xml", "application/javascript",
		"application/x-yaml", "application/yaml", "application/x-sh", "application/sql",
	} {
		if mt == m || strings.HasPrefix(mt, m+";") {
			return true
		}
	}
	return false
}

type attachmentArgs struct {
	id             string
	filename       string
	mimeType       string
	buffer         []byte
	maxDimension   *int
	quality        *int
	frames         *int
	start          *float64
	end            *float64
	mode           string
	sceneThreshold *float64
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func imageHasAlpha(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	return false
}

// processImage resizes (long edge → maxDim) and re-encodes: PNG for images with
// alpha, JPEG otherwise. SVG passes through untouched.
func processImage(buffer []byte, mimeType string, maxDim, quality int) (data []byte, outMime string, resized bool, err error) {
	if strings.ToLower(mimeType) == "image/svg+xml" {
		return buffer, mimeType, false, nil
	}
	img, err := imaging.Decode(bytes.NewReader(buffer), imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", false, err
	}
	b := img.Bounds()
	longEdge := b.Dx()
	if b.Dy() > longEdge {
		longEdge = b.Dy()
	}
	needsResize := longEdge > maxDim
	out := img
	if needsResize {
		out = imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)
	}
	var buf bytes.Buffer
	if imageHasAlpha(img) {
		if err := imaging.Encode(&buf, out, imaging.PNG, imaging.PNGCompressionLevel(png.BestCompression)); err != nil {
			return nil, "", false, err
		}
		return buf.Bytes(), "image/png", needsResize, nil
	}
	if err := imaging.Encode(&buf, out, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
		return nil, "", false, err
	}
	return buf.Bytes(), "image/jpeg", needsResize, nil
}

var pngSig = []byte{0x89, 0x50, 0x4E, 0x47}

func isAnimatedImage(buffer []byte, mimeType string) bool {
	mt := strings.ToLower(mimeType)
	if strings.HasPrefix(mt, "image/gif") || (len(buffer) >= 3 && string(buffer[:3]) == "GIF") {
		if g, err := gif.DecodeAll(bytes.NewReader(buffer)); err == nil && len(g.Image) > 1 {
			return true
		}
	}
	if bytes.HasPrefix(buffer, pngSig) && bytes.Contains(buffer, []byte("acTL")) {
		return true
	}
	if len(buffer) >= 12 && string(buffer[:4]) == "RIFF" && string(buffer[8:12]) == "WEBP" && bytes.Contains(buffer, []byte("ANIM")) {
		return true
	}
	return false
}

// ── temp-file retention ──────────────────────────────────────────────────────

const tmpPrefix = "atlmcp-"
const tmpPruneCooldown = time.Hour

var (
	pruneMu     sync.Mutex
	lastPruneAt time.Time
)

func tmpTTL() time.Duration {
	if v := os.Getenv("ATLASSIAN_MCP_TMP_TTL_DAYS"); v != "" {
		if d, err := strconv.ParseFloat(v, 64); err == nil && d > 0 {
			return time.Duration(d * 24 * float64(time.Hour))
		}
	}
	return 7 * 24 * time.Hour
}

func tmpMaxBytes() int64 {
	if v := os.Getenv("ATLASSIAN_MCP_TMP_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 1024 * 1024 * 1024
}

func pruneTmpFiles() {
	pruneMu.Lock()
	defer pruneMu.Unlock()
	now := time.Now()
	if !lastPruneAt.IsZero() && now.Sub(lastPruneAt) < tmpPruneCooldown {
		return
	}
	lastPruneAt = now
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	ttl := tmpTTL()
	type survivor struct {
		path  string
		size  int64
		mtime time.Time
	}
	var survivors []survivor
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, tmpPrefix) {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil || info.IsDir() {
			continue
		}
		if now.Sub(info.ModTime()) > ttl {
			os.Remove(p)
			continue
		}
		survivors = append(survivors, survivor{p, info.Size(), info.ModTime()})
	}
	var total int64
	for _, s := range survivors {
		total += s.size
	}
	maxBytes := tmpMaxBytes()
	if total > maxBytes {
		sort.Slice(survivors, func(i, j int) bool { return survivors[i].mtime.Before(survivors[j].mtime) })
		for _, s := range survivors {
			if total <= maxBytes {
				break
			}
			if os.Remove(s.path) == nil {
				total -= s.size
			}
		}
	}
}

var sanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func sanitizeFilename(name string) string {
	s := sanitizeRe.ReplaceAllString(name, "_")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		return "attachment"
	}
	return s
}

func autoSaveOversized(id, filename string, buffer []byte) (string, error) {
	pruneTmpFiles()
	path := filepath.Join(os.TempDir(), tmpPrefix+id+"-"+sanitizeFilename(filename))
	if err := os.WriteFile(path, buffer, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ── main dispatch ────────────────────────────────────────────────────────────

func buildAttachmentResult(a attachmentArgs) (toolResult, error) {
	sizeLabel := formatBytes(int64(len(a.buffer)))
	header := fmt.Sprintf("%s — %s, %s", a.filename, a.mimeType, sizeLabel)
	mt := strings.ToLower(a.mimeType)

	switch {
	case strings.HasPrefix(mt, "image/"):
		if isAnimatedImage(a.buffer, a.mimeType) {
			if int64(len(a.buffer)) > maxVideoSourceBytes {
				path, err := autoSaveOversized(a.id, a.filename, a.buffer)
				if err != nil {
					return toolResult{}, err
				}
				return textResult(fmt.Sprintf("%s\nAnimated image exceeds %s processing cap. Original saved to %s.", header, formatBytes(maxVideoSourceBytes), path)), nil
			}
			return buildVideoResult(a, header, "animated image"), nil
		}
		if int64(len(a.buffer)) > maxInlineBytes {
			path, err := autoSaveOversized(a.id, a.filename, a.buffer)
			if err != nil {
				return toolResult{}, err
			}
			return textResult(fmt.Sprintf("%s\nImage exceeds %s inline cap. Original saved to %s.", header, formatBytes(maxInlineBytes), path)), nil
		}
		maxDim := intOr(a.maxDimension, defaultMaxDimension)
		quality := intOr(a.quality, defaultJpegQuality)
		data, outMime, resized, err := processImage(a.buffer, a.mimeType, maxDim, quality)
		if err != nil {
			// Pure-Go decoders don't handle AVIF/HEIC/JPEG-XL; fall back to a
			// one-shot ffmpeg still-decode (matches what sharp could decode).
			if raw, ferr := decodeStillViaFFmpeg(a.buffer); ferr == nil {
				if d2, m2, r2, e2 := processImage(raw, "image/png", maxDim, quality); e2 == nil {
					data, outMime, resized, err = d2, m2, r2, nil
				}
			}
		}
		if err != nil {
			return textResult(fmt.Sprintf("%s\nFailed to process image: %s. Pass saveTo to write the original to disk.", header, err.Error())), nil
		}
		note := ""
		if resized {
			note = fmt.Sprintf(" (resized to %dpx long edge, re-encoded to %s)", maxDim, formatBytes(int64(len(data))))
		} else if len(data) < len(a.buffer) {
			note = fmt.Sprintf(" (re-encoded to %s)", formatBytes(int64(len(data))))
		}
		return toolResult{Content: []contentBlock{
			{Type: "text", Text: fmt.Sprintf("Attachment #%s: %s%s", a.id, header, note)},
			{Type: "image", Data: b64(data), MimeType: outMime},
		}}, nil

	case strings.HasPrefix(mt, "video/"):
		if int64(len(a.buffer)) > maxVideoSourceBytes {
			path, err := autoSaveOversized(a.id, a.filename, a.buffer)
			if err != nil {
				return toolResult{}, err
			}
			return textResult(fmt.Sprintf("%s\nVideo exceeds %s processing cap. Original saved to %s.", header, formatBytes(maxVideoSourceBytes), path)), nil
		}
		return buildVideoResult(a, header, "video"), nil

	case strings.HasPrefix(mt, "audio/"):
		if int64(len(a.buffer)) > maxInlineBytes {
			path, err := autoSaveOversized(a.id, a.filename, a.buffer)
			if err != nil {
				return toolResult{}, err
			}
			return textResult(fmt.Sprintf("%s\nAudio exceeds %s inline cap. Original saved to %s.", header, formatBytes(maxInlineBytes), path)), nil
		}
		return toolResult{Content: []contentBlock{
			{Type: "text", Text: fmt.Sprintf("Attachment #%s: %s", a.id, header)},
			{Type: "audio", Data: b64(a.buffer), MimeType: mt},
		}}, nil

	case mt == "application/pdf":
		if int64(len(a.buffer)) > maxInlineBytes {
			path, err := autoSaveOversized(a.id, a.filename, a.buffer)
			if err != nil {
				return toolResult{}, err
			}
			return textResult(fmt.Sprintf("%s\nPDF exceeds %s inline cap. Original saved to %s.", header, formatBytes(maxInlineBytes), path)), nil
		}
		return buildPDFResult(a, header), nil

	case isTextMime(mt) && int64(len(a.buffer)) <= maxInlineBytes:
		return textResult(fmt.Sprintf("Attachment #%s: %s\n\n%s", a.id, header, string(a.buffer))), nil
	}

	path, err := autoSaveOversized(a.id, a.filename, a.buffer)
	if err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("%s\nAttachment #%s is not inline-renderable. Original saved to %s.", header, a.id, path)), nil
}

func buildVideoResult(a attachmentArgs, header, sourceLabel string) toolResult {
	maxDim := intOr(a.maxDimension, defaultVideoMaxDimension)
	quality := intOr(a.quality, defaultVideoQuality)
	frames := intOr(a.frames, defaultVideoFrames)
	mode := a.mode
	if mode == "" {
		mode = "uniform"
	}
	sceneThreshold := defaultSceneThreshold
	if a.sceneThreshold != nil {
		sceneThreshold = *a.sceneThreshold
	}
	result, err := processVideo(a.buffer, processVideoOpts{
		frames:         frames,
		start:          a.start,
		end:            a.end,
		dedup:          true,
		mode:           mode,
		sceneThreshold: sceneThreshold,
	})
	if err != nil {
		return textResult(fmt.Sprintf("%s\nFailed to process %s: %s. Pass saveTo=/absolute/path to write the original to disk.", header, sourceLabel, err.Error()))
	}
	if len(result.frames) == 0 {
		return textResult(fmt.Sprintf("%s\nNo frames extracted from %s. Pass saveTo=/absolute/path to write the original to disk.", header, sourceLabel))
	}
	m := result.meta
	tsNote := ""
	if result.approximateTimestamps {
		tsNote = " (timestamps approximate)"
	}
	dedupNote := ""
	if result.dedupApplied {
		dedupNote = " (mpdecimate enabled)"
	}
	summary := strings.Join([]string{
		fmt.Sprintf("Attachment #%s: %s", a.id, header),
		fmt.Sprintf("Duration: %.1fs, %d×%d @ %.1ffps, codec %s", m.duration, m.width, m.height, m.fps, m.codec),
		fmt.Sprintf("Sampled %d frame(s) via %s mode from %.1fs–%.1fs at %dpx / q%d%s%s.", len(result.frames), result.mode, result.effectiveStart, result.effectiveEnd, maxDim, quality, dedupNote, tsNote),
		"Re-call with start=<sec> end=<sec> frames=<n> or mode=scenes to refine.",
	}, "\n")

	content := []contentBlock{{Type: "text", Text: summary}}
	for i, f := range result.frames {
		data := reencodeFrame(f.data, maxDim, quality)
		tsPrefix := ""
		if f.approximate {
			tsPrefix = "~"
		}
		content = append(content,
			contentBlock{Type: "text", Text: fmt.Sprintf("Frame %d @ %s%.2fs (%s):", i+1, tsPrefix, f.timestampSec, formatBytes(int64(len(data))))},
			contentBlock{Type: "image", Data: b64(data), MimeType: "image/jpeg"})
	}
	return toolResult{Content: content}
}

func reencodeFrame(frame []byte, maxDim, quality int) []byte {
	img, err := imaging.Decode(bytes.NewReader(frame), imaging.AutoOrientation(true))
	if err != nil {
		return frame
	}
	out := imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)
	var buf bytes.Buffer
	if imaging.Encode(&buf, out, imaging.JPEG, imaging.JPEGQuality(quality)) != nil {
		return frame
	}
	return buf.Bytes()
}

// ── PDF ──────────────────────────────────────────────────────────────────────

const pdfRasterThreshold = 20
const pdfMaxRasterPages = 3

func extractPDFText(buffer []byte) (totalPages int, body string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	r, perr := pdf.NewReader(bytes.NewReader(buffer), int64(len(buffer)))
	if perr != nil {
		return 0, "", perr
	}
	totalPages = r.NumPage()
	var sb strings.Builder
	for i := 1; i <= totalPages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		txt, terr := p.GetPlainText(nil)
		if terr != nil {
			continue
		}
		if i > 1 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(txt)
	}
	return totalPages, strings.TrimSpace(sb.String()), nil
}

func buildPDFResult(a attachmentArgs, header string) toolResult {
	totalPages, body, err := extractPDFText(a.buffer)
	if err != nil {
		path, serr := autoSaveOversized(a.id, a.filename, a.buffer)
		if serr != nil {
			return textResult(fmt.Sprintf("%s\nFailed to extract PDF text: %s.", header, err.Error()))
		}
		return textResult(fmt.Sprintf("%s\nFailed to extract PDF text: %s. Original saved to %s.", header, err.Error(), path))
	}

	if len(body) < pdfRasterThreshold && totalPages > 0 {
		pageCount := totalPages
		if pageCount > pdfMaxRasterPages {
			pageCount = pdfMaxRasterPages
		}
		maxDim := intOr(a.maxDimension, defaultMaxDimension)
		quality := intOr(a.quality, defaultJpegQuality)
		images, rerr := rasterizePDF(a.buffer, pageCount, maxDim)
		if rerr != nil || len(images) == 0 {
			path, _ := autoSaveOversized(a.id, a.filename, a.buffer)
			reason := "no external PDF rasterizer (pdftoppm/mutool) found"
			if rerr != nil {
				reason = rerr.Error()
			}
			return textResult(fmt.Sprintf("%s\nNo extractable text and rasterization failed: %s. Original saved to %s.", header, reason, path))
		}
		content := []contentBlock{{Type: "text", Text: fmt.Sprintf("Attachment #%s: %s\nNo extractable text found (likely scanned). Rasterized first %d of %d page(s):", a.id, header, len(images), totalPages)}}
		for i, raw := range images {
			data := reencodeFrame(raw, maxDim, quality)
			content = append(content,
				contentBlock{Type: "text", Text: fmt.Sprintf("Page %d (%s):", i+1, formatBytes(int64(len(data))))},
				contentBlock{Type: "image", Data: b64(data), MimeType: "image/jpeg"})
		}
		return toolResult{Content: content}
	}

	return textResult(fmt.Sprintf("Attachment #%s: %s\nExtracted text from %d page(s):\n\n%s", a.id, header, totalPages, body))
}

// rasterizePDF renders the first pageCount pages to raster images using an
// external tool (pdftoppm or mutool), returning the raw image bytes per page.
func rasterizePDF(buffer []byte, pageCount, maxDim int) ([][]byte, error) {
	dir, err := os.MkdirTemp(os.TempDir(), "atlmcp-pdf-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	inputPDF := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(inputPDF, buffer, 0o644); err != nil {
		return nil, err
	}

	if p, lerr := exec.LookPath("pdftoppm"); lerr == nil {
		prefix := filepath.Join(dir, "page")
		cmd := exec.Command(p, "-jpeg", "-scale-to", strconv.Itoa(maxDim), "-f", "1", "-l", strconv.Itoa(pageCount), inputPDF, prefix)
		if err := cmd.Run(); err == nil {
			if imgs := collectImages(dir, "page", []string{".jpg", ".jpeg"}); len(imgs) > 0 {
				return imgs, nil
			}
		}
	}
	if p, lerr := exec.LookPath("mutool"); lerr == nil {
		out := filepath.Join(dir, "page-%d.png")
		cmd := exec.Command(p, "draw", "-F", "png", "-w", strconv.Itoa(maxDim), "-o", out, inputPDF, fmt.Sprintf("1-%d", pageCount))
		if err := cmd.Run(); err == nil {
			if imgs := collectImages(dir, "page-", []string{".png"}); len(imgs) > 0 {
				return imgs, nil
			}
		}
	}
	return nil, fmt.Errorf("no external PDF rasterizer (pdftoppm/mutool) found on PATH")
}

func collectImages(dir, prefix string, exts []string) [][]byte {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		for _, ext := range exts {
			if strings.HasSuffix(strings.ToLower(n), ext) {
				names = append(names, n)
				break
			}
		}
	}
	sort.Strings(names)
	var out [][]byte
	for _, n := range names {
		if data, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			out = append(out, data)
		}
	}
	return out
}

// getAttachmentDispatch is the get_attachment tool entry: one tool over both
// services, since the two only differ in how the bytes are fetched. source is
// optional when only one service is configured — there is nothing to disambiguate.
func getAttachmentDispatch(session *sessionState, args map[string]any) (toolResult, error) {
	args = normalizeBitbucketArgs(args)
	if rerr := validateAttachmentArgs(args); rerr != nil {
		return toolResult{}, rerr
	}
	source := argString(args, "source")
	if source == "" {
		switch {
		case jira != nil && bitbucket == nil:
			source = "jira"
		case bitbucket != nil && jira == nil:
			source = "bitbucket"
		default:
			return toolResult{}, fmt.Errorf("source is required: \"jira\" for an issue attachment (IDs from jira_get) or \"bitbucket\" for a repo attachment (IDs from bitbucket_get_pr).")
		}
	}
	if source == "jira" {
		if jira == nil {
			return toolResult{}, fmt.Errorf("Jira is not configured.")
		}
		return jira.getAttachment(args)
	}
	if bitbucket == nil {
		return toolResult{}, fmt.Errorf("Bitbucket is not configured.")
	}
	return bitbucket.getAttachment(args, resolveRepoRoot(session, args))
}
