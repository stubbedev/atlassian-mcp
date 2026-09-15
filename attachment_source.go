package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// An attachments entry is a *source*, not necessarily a path on this machine.
// A local path only works when the server shares a filesystem with whoever named
// the file and is allowed to read it — not a given on a host that confines the
// server. A URL or a data: URI needs neither, so every upload path resolves its
// sources through here before touching the multipart writer.

const maxRemoteAttachmentBytes = maxVideoSourceBytes

const attachmentSourceHint = "Sources may be a local file path, an http(s) URL (fetched here — Jira/Bitbucket URLs are sent with your token), or a data:<mime>;base64,<...> URI."

// resolvedAttachment is one upload source reduced to bytes on disk. Anything
// that was not already a local file lands in a temp file that
// releaseAttachments removes once the upload has been sent.
type resolvedAttachment struct {
	path string // local file holding the bytes
	name string // filename to upload under
	ref  string // the source as the caller spelled it, for markup splicing
	tmp  bool   // path is a temp file this process created
}

// resolveAttachmentSources resolves every entry, cleaning up after itself if any
// one of them fails — a half-resolved batch must not leak temp files.
func resolveAttachmentSources(sources []string) ([]resolvedAttachment, error) {
	out := make([]resolvedAttachment, 0, len(sources))
	for _, s := range sources {
		r, err := resolveAttachmentSource(strings.TrimSpace(s))
		if err != nil {
			releaseAttachments(out)
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// label names the source in errors: the caller's spelling when there is one,
// the filename for inlined bytes that have no meaningful spelling.
func (r resolvedAttachment) label() string {
	if r.ref != "" {
		return r.ref
	}
	return r.name
}

func releaseAttachments(list []resolvedAttachment) {
	for _, r := range list {
		if r.tmp {
			_ = os.Remove(r.path)
		}
	}
}

func resolveAttachmentSource(src string) (resolvedAttachment, error) {
	if src == "" {
		return resolvedAttachment{}, fmt.Errorf("empty attachment source. %s", attachmentSourceHint)
	}
	switch {
	case strings.HasPrefix(strings.ToLower(src), "data:"):
		return resolveDataURIAttachment(src)
	case hasHTTPScheme(src):
		return fetchRemoteAttachment(src)
	case strings.HasPrefix(strings.ToLower(src), "file://"):
		p := fileURIToPath(src)
		if p == "" {
			return resolvedAttachment{}, fmt.Errorf("cannot read attachment %s: not a valid file URI.", src)
		}
		return resolveLocalAttachment(p, src)
	}
	return resolveLocalAttachment(src, src)
}

func hasHTTPScheme(src string) bool {
	l := strings.ToLower(src)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

func resolveLocalAttachment(p, ref string) (resolvedAttachment, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	info, err := os.Stat(abs)
	if err != nil {
		// A permission error on a path that exists is how a filesystem sandbox
		// shows up here, so name the alternatives that do not need one.
		return resolvedAttachment{}, fmt.Errorf("cannot read attachment %s: %w. If this server runs confined (e.g. as a Claude Desktop extension) it may not be allowed to read this path. %s", p, err, attachmentSourceHint)
	}
	if info.IsDir() {
		return resolvedAttachment{}, fmt.Errorf("cannot attach %s: it is a directory.", p)
	}
	return resolvedAttachment{path: abs, name: filepath.Base(abs), ref: ref}, nil
}

// resolveDataURIAttachment decodes data:[<mime>][;name=<fn>][;base64],<data>.
// The name parameter is not in RFC 2397 but is widely used and is the only way
// a caller can title bytes it inlined.
func resolveDataURIAttachment(src string) (resolvedAttachment, error) {
	comma := strings.IndexByte(src, ',')
	if comma < 0 {
		return resolvedAttachment{}, fmt.Errorf("malformed data: URI — no comma separating the header from the payload. %s", attachmentSourceHint)
	}
	header, payload := src[len("data:"):comma], src[comma+1:]
	parts := strings.Split(header, ";")
	mediaType := ""
	name := ""
	isBase64 := false
	for i, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case strings.EqualFold(p, "base64"):
			isBase64 = true
		case strings.HasPrefix(strings.ToLower(p), "name="):
			name = strings.Trim(p[len("name="):], `"`)
		case i == 0:
			mediaType = strings.ToLower(p)
		}
	}
	var data []byte
	var err error
	if isBase64 {
		// Tolerate whitespace and both alphabets: models wrap long base64.
		cleaned := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(payload)
		if data, err = base64.StdEncoding.DecodeString(cleaned); err != nil {
			if data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(cleaned, "=")); err != nil {
				return resolvedAttachment{}, fmt.Errorf("cannot decode base64 data: URI: %w", err)
			}
		}
	} else {
		unescaped, uerr := url.PathUnescape(payload)
		if uerr != nil {
			return resolvedAttachment{}, fmt.Errorf("cannot decode data: URI payload: %w", uerr)
		}
		data = []byte(unescaped)
	}
	if len(data) == 0 {
		return resolvedAttachment{}, errors.New("data: URI decoded to zero bytes.")
	}
	if int64(len(data)) > maxRemoteAttachmentBytes {
		return resolvedAttachment{}, fmt.Errorf("data: URI is %s, exceeds the %s attachment cap.", formatBytes(int64(len(data))), formatBytes(maxRemoteAttachmentBytes))
	}
	if name == "" {
		name = "attachment" + extensionForMediaType(mediaType)
	}
	p, err := writeTempAttachment(name, func(w io.Writer) (int64, error) {
		n, werr := w.Write(data)
		return int64(n), werr
	})
	if err != nil {
		return resolvedAttachment{}, err
	}
	// The ref is the whole (possibly enormous) URI; splicing it into PR text
	// would be absurd, so leave it empty and let the markup be appended.
	return resolvedAttachment{path: p, name: sanitizeFilename(name), tmp: true}, nil
}

// fetchRemoteAttachment downloads a URL into a temp file. Requests to the
// configured Jira/Bitbucket hosts carry the bearer token, so an attachment
// already living on one service can be re-attached to the other by URL.
func fetchRemoteAttachment(src string) (resolvedAttachment, error) {
	u, err := url.Parse(src)
	if err != nil {
		return resolvedAttachment{}, fmt.Errorf("cannot fetch attachment %s: %w", src, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return resolvedAttachment{}, fmt.Errorf("cannot fetch attachment %s: %w", src, err)
	}
	req.Header.Set("Accept", "*/*")
	if tok := attachmentAuthToken(u); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return resolvedAttachment{}, fmt.Errorf("cannot fetch attachment %s: %w", src, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		detail := strings.TrimSpace(string(snippet))
		if detail != "" {
			detail = ": " + detail
		}
		return resolvedAttachment{}, fmt.Errorf("cannot fetch attachment %s: HTTP %d%s", src, res.StatusCode, detail)
	}

	name := remoteAttachmentName(res, u)
	var written int64
	p, err := writeTempAttachment(name, func(w io.Writer) (int64, error) {
		// One byte over the cap is enough to detect it without buffering more.
		return io.Copy(w, io.LimitReader(res.Body, maxRemoteAttachmentBytes+1))
	})
	if err != nil {
		return resolvedAttachment{}, err
	}
	if info, serr := os.Stat(p); serr == nil {
		written = info.Size()
	}
	if written > maxRemoteAttachmentBytes {
		_ = os.Remove(p)
		return resolvedAttachment{}, fmt.Errorf("attachment %s exceeds the %s cap.", src, formatBytes(maxRemoteAttachmentBytes))
	}
	if written == 0 {
		_ = os.Remove(p)
		return resolvedAttachment{}, fmt.Errorf("attachment %s downloaded zero bytes.", src)
	}
	return resolvedAttachment{path: p, name: name, ref: src, tmp: true}, nil
}

// attachmentAuthToken returns the bearer token for a URL that points at one of
// the configured services. Host must match exactly — a token is never sent
// anywhere the server was not already talking to.
func attachmentAuthToken(u *url.URL) string {
	if jira != nil {
		if sameHost(u, jira.baseURL) {
			return jira.token
		}
	}
	if bitbucket != nil {
		if sameHost(u, bitbucket.baseURL) {
			return bitbucket.token
		}
	}
	return ""
}

func sameHost(u *url.URL, base string) bool {
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, b.Host)
}

// remoteAttachmentName picks the filename: Content-Disposition first, then the
// URL's last path segment, and an extension from Content-Type when the name
// carries none (Bitbucket's attachment URLs end in a bare numeric id).
func remoteAttachmentName(res *http.Response, u *url.URL) string {
	name := ""
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			name = params["filename"]
		}
	}
	if name == "" {
		if base := path.Base(u.Path); base != "." && base != "/" && base != "" {
			if unescaped, err := url.PathUnescape(base); err == nil {
				base = unescaped
			}
			name = base
		}
	}
	if name == "" {
		name = "attachment"
	}
	if filepath.Ext(name) == "" {
		mediaType := res.Header.Get("Content-Type")
		if ct, _, err := mime.ParseMediaType(mediaType); err == nil {
			mediaType = ct
		}
		name += extensionForMediaType(mediaType)
	}
	return sanitizeFilename(name)
}

// extensionForMediaType maps a MIME type to a file extension. The common image
// types are pinned because mime.ExtensionsByType sorts alphabetically and would
// hand back ".jfif" for image/jpeg on some systems.
func extensionForMediaType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "":
		return ""
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// writeTempAttachment streams bytes into a temp file named so that a leaked one
// is still swept by pruneTmpFiles.
func writeTempAttachment(name string, write func(io.Writer) (int64, error)) (string, error) {
	f, err := os.CreateTemp(os.TempDir(), tmpPrefix+"upload-*-"+sanitizeFilename(name))
	if err != nil {
		return "", fmt.Errorf("cannot buffer attachment %s: %w", name, err)
	}
	if _, err := write(f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("cannot buffer attachment %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("cannot buffer attachment %s: %w", name, err)
	}
	return f.Name(), nil
}
