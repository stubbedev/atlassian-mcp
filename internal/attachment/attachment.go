package attachment

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stubbedev/atlassian-mcp/internal/media"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

const (
	maxInlineBytes      = 10 * 1024 * 1024
	defaultMaxDimension = 1568
	defaultJpegQuality  = 85
)

func FormatBytes(b int64) string {
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

type Args struct {
	ID             string
	Filename       string
	MimeType       string
	Buffer         []byte
	MaxDimension   *int
	Quality        *int
	Frames         *int
	Start          *float64
	End            *float64
	Mode           string
	SceneThreshold *float64
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
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
			_ = os.Remove(p)
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
	if err := os.WriteFile(path, buffer, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ── main dispatch ────────────────────────────────────────────────────────────

func BuildResult(a Args) (tool.Result, error) {
	sizeLabel := FormatBytes(int64(len(a.Buffer)))
	header := fmt.Sprintf("%s — %s, %s", a.Filename, a.MimeType, sizeLabel)
	mt := strings.ToLower(a.MimeType)

	switch {
	case strings.HasPrefix(mt, "image/"):
		if media.IsAnimatedImage(a.Buffer, a.MimeType) {
			if int64(len(a.Buffer)) > media.MaxVideoSourceBytes {
				path, err := autoSaveOversized(a.ID, a.Filename, a.Buffer)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.TextResult(fmt.Sprintf("%s\nAnimated image exceeds %s processing cap. Original saved to %s.", header, FormatBytes(media.MaxVideoSourceBytes), path)), nil
			}
			return buildVideoResult(a, header, "animated image"), nil
		}
		if int64(len(a.Buffer)) > maxInlineBytes {
			path, err := autoSaveOversized(a.ID, a.Filename, a.Buffer)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.TextResult(fmt.Sprintf("%s\nImage exceeds %s inline cap. Original saved to %s.", header, FormatBytes(maxInlineBytes), path)), nil
		}
		maxDim := intOr(a.MaxDimension, defaultMaxDimension)
		quality := intOr(a.Quality, defaultJpegQuality)
		data, outMime, resized, err := media.ProcessImage(a.Buffer, a.MimeType, maxDim, quality)
		if err != nil {
			return tool.TextResult(fmt.Sprintf("%s\nFailed to process image: %s. Pass saveTo to write the original to disk.", header, err.Error())), nil
		}
		note := ""
		if resized {
			note = fmt.Sprintf(" (resized to %dpx long edge, re-encoded to %s)", maxDim, FormatBytes(int64(len(data))))
		} else if len(data) < len(a.Buffer) {
			note = fmt.Sprintf(" (re-encoded to %s)", FormatBytes(int64(len(data))))
		}
		return tool.Result{Content: []tool.ContentBlock{
			{Type: "text", Text: fmt.Sprintf("Attachment #%s: %s%s", a.ID, header, note)},
			{Type: "image", Data: b64(data), MimeType: outMime},
		}}, nil

	case strings.HasPrefix(mt, "video/"):
		if int64(len(a.Buffer)) > media.MaxVideoSourceBytes {
			path, err := autoSaveOversized(a.ID, a.Filename, a.Buffer)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.TextResult(fmt.Sprintf("%s\nVideo exceeds %s processing cap. Original saved to %s.", header, FormatBytes(media.MaxVideoSourceBytes), path)), nil
		}
		return buildVideoResult(a, header, "video"), nil

	case strings.HasPrefix(mt, "audio/"):
		if int64(len(a.Buffer)) > maxInlineBytes {
			path, err := autoSaveOversized(a.ID, a.Filename, a.Buffer)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.TextResult(fmt.Sprintf("%s\nAudio exceeds %s inline cap. Original saved to %s.", header, FormatBytes(maxInlineBytes), path)), nil
		}
		return tool.Result{Content: []tool.ContentBlock{
			{Type: "text", Text: fmt.Sprintf("Attachment #%s: %s", a.ID, header)},
			{Type: "audio", Data: b64(a.Buffer), MimeType: mt},
		}}, nil

	case mt == "application/pdf":
		if int64(len(a.Buffer)) > maxInlineBytes {
			path, err := autoSaveOversized(a.ID, a.Filename, a.Buffer)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.TextResult(fmt.Sprintf("%s\nPDF exceeds %s inline cap. Original saved to %s.", header, FormatBytes(maxInlineBytes), path)), nil
		}
		return buildPDFResult(a, header), nil

	case isTextMime(mt) && int64(len(a.Buffer)) <= maxInlineBytes:
		return tool.TextResult(fmt.Sprintf("Attachment #%s: %s\n\n%s", a.ID, header, string(a.Buffer))), nil
	}

	path, err := autoSaveOversized(a.ID, a.Filename, a.Buffer)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.TextResult(fmt.Sprintf("%s\nAttachment #%s is not inline-renderable. Original saved to %s.", header, a.ID, path)), nil
}

func buildVideoResult(a Args, header, sourceLabel string) tool.Result {
	maxDim := intOr(a.MaxDimension, media.DefaultVideoMaxDimension)
	quality := intOr(a.Quality, media.DefaultVideoQuality)
	frames := intOr(a.Frames, media.DefaultVideoFrames)
	mode := a.Mode
	if mode == "" {
		mode = "uniform"
	}
	sceneThreshold := media.DefaultSceneThreshold
	if a.SceneThreshold != nil {
		sceneThreshold = *a.SceneThreshold
	}
	result, err := media.ProcessVideo(a.Buffer, media.ProcessVideoOpts{
		Frames:         frames,
		Start:          a.Start,
		End:            a.End,
		Dedup:          true,
		Mode:           mode,
		SceneThreshold: sceneThreshold,
	})
	if err != nil {
		return tool.TextResult(fmt.Sprintf("%s\nFailed to process %s: %s. Pass saveTo=/absolute/path to write the original to disk.", header, sourceLabel, err.Error()))
	}
	if len(result.Frames) == 0 {
		return tool.TextResult(fmt.Sprintf("%s\nNo frames extracted from %s. Pass saveTo=/absolute/path to write the original to disk.", header, sourceLabel))
	}
	m := result.Meta
	tsNote := ""
	if result.ApproximateTimestamps {
		tsNote = " (timestamps approximate)"
	}
	dedupNote := ""
	if result.DedupApplied {
		dedupNote = " (near-duplicate frames dropped)"
	}
	summaryLines := []string{
		fmt.Sprintf("Attachment #%s: %s", a.ID, header),
		fmt.Sprintf("Duration: %.1fs, %d×%d @ %.1ffps, codec %s", m.Duration, m.Width, m.Height, m.FPS, m.Codec),
		fmt.Sprintf("Sampled %d frame(s) via %s mode from %.1fs–%.1fs at %dpx / q%d%s%s.", len(result.Frames), result.Mode, result.EffectiveStart, result.EffectiveEnd, maxDim, quality, dedupNote, tsNote),
	}
	if result.Note != "" {
		summaryLines = append(summaryLines, result.Note)
	}
	summary := strings.Join(append(summaryLines, "Re-call with start=<sec> end=<sec> frames=<n> or mode=scenes to refine."), "\n")

	content := []tool.ContentBlock{{Type: "text", Text: summary}}
	for i, f := range result.Frames {
		data := media.ReencodeFrame(f.Data, maxDim, quality)
		tsPrefix := ""
		if f.Approximate {
			tsPrefix = "~"
		}
		content = append(content,
			tool.ContentBlock{Type: "text", Text: fmt.Sprintf("Frame %d @ %s%.2fs (%s):", i+1, tsPrefix, f.TimestampSec, FormatBytes(int64(len(data))))},
			tool.ContentBlock{Type: "image", Data: b64(data), MimeType: "image/jpeg"})
	}
	return tool.Result{Content: content}
}

func buildPDFResult(a Args, header string) tool.Result {
	totalPages, body, err := media.ExtractPDFText(a.Buffer)
	if err != nil {
		path, serr := autoSaveOversized(a.ID, a.Filename, a.Buffer)
		if serr != nil {
			return tool.TextResult(fmt.Sprintf("%s\nFailed to extract PDF text: %s.", header, err.Error()))
		}
		return tool.TextResult(fmt.Sprintf("%s\nFailed to extract PDF text: %s. Original saved to %s.", header, err.Error(), path))
	}

	if len(body) < media.PDFRasterThreshold && totalPages > 0 {
		pageCount := min(totalPages, media.PDFMaxRasterPages)
		maxDim := intOr(a.MaxDimension, defaultMaxDimension)
		quality := intOr(a.Quality, defaultJpegQuality)
		images, rerr := media.RasterizePDF(a.Buffer, pageCount, maxDim)
		if rerr != nil || len(images) == 0 {
			path, _ := autoSaveOversized(a.ID, a.Filename, a.Buffer)
			reason := "the PDF rendered no pages"
			if rerr != nil {
				reason = rerr.Error()
			}
			return tool.TextResult(fmt.Sprintf("%s\nNo extractable text and rasterization failed: %s. Original saved to %s.", header, reason, path))
		}
		content := []tool.ContentBlock{{Type: "text", Text: fmt.Sprintf("Attachment #%s: %s\nNo extractable text found (likely scanned). Rasterized first %d of %d page(s):", a.ID, header, len(images), totalPages)}}
		for i, img := range images {
			data := media.EncodeFittedJPEG(img, maxDim, quality)
			content = append(content,
				tool.ContentBlock{Type: "text", Text: fmt.Sprintf("Page %d (%s):", i+1, FormatBytes(int64(len(data))))},
				tool.ContentBlock{Type: "image", Data: b64(data), MimeType: "image/jpeg"})
		}
		return tool.Result{Content: content}
	}

	return tool.TextResult(fmt.Sprintf("Attachment #%s: %s\nExtracted text from %d page(s):\n\n%s", a.ID, header, totalPages, body))
}
