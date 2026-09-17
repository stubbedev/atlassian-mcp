package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Bitbucket Server stores attachments per repository, not per PR: a file is
// uploaded once to the repo and then referenced from a description or comment
// as ![name](attachment:<repoId>/<id>). Only that markup makes it visible, so every
// upload here also gets spliced into the text it belongs to.

type bbUploadedAttachment struct {
	ID    json.Number `json:"id"`
	URL   string      `json:"url"`
	Name  string      `json:"name"`
	Links struct {
		Attachment struct {
			Href string `json:"href"`
		} `json:"attachment"`
	} `json:"links"`
	ref string // the source as the caller spelled it, for markup splicing
}

// link is what goes in the markdown link target. Bitbucket's own markup is
// attachment:<repoId>/<id>, handed back as links.attachment.href — the bare
// attachment:<id> the id alone would spell renders to an empty src, so the
// href is preferred and the absolute URL is the only fallback worth writing.
func (a bbUploadedAttachment) link() string {
	if h := a.Links.Attachment.Href; h != "" {
		return h
	}
	if a.URL != "" {
		return a.URL
	}
	return "attachment:" + a.ID.String()
}

// markup renders the reference: an image inlines, anything else is a plain link.
func (a bbUploadedAttachment) markup() string {
	bang := ""
	if strings.HasPrefix(mime.TypeByExtension(strings.ToLower(filepath.Ext(a.Name))), "image/") {
		bang = "!"
	}
	return bang + "[" + a.Name + "](" + a.link() + ")"
}

// uploadAttachments resolves each source (local path, URL, data: URI) and POSTs
// it to the repo attachments endpoint. One request per file: Bitbucket takes
// several files per request but returns them in an array that two files with
// the same base name make ambiguous.
func (c *BitbucketClient) uploadAttachments(projectKey, repoSlug string, sources []string) ([]bbUploadedAttachment, error) {
	resolved, err := resolveAttachmentSources(sources)
	if err != nil {
		return nil, err
	}
	defer releaseAttachments(resolved)

	out := make([]bbUploadedAttachment, 0, len(resolved))
	for _, r := range resolved {
		a, err := c.uploadAttachment(projectKey, repoSlug, r)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (c *BitbucketClient) uploadAttachment(projectKey, repoSlug string, src resolvedAttachment) (bbUploadedAttachment, error) {
	f, err := os.Open(src.path)
	if err != nil {
		return bbUploadedAttachment{}, fmt.Errorf("cannot open attachment %s: %w", src.label(), err)
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("files", src.name)
	if err != nil {
		return bbUploadedAttachment{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return bbUploadedAttachment{}, err
	}
	if err := w.Close(); err != nil {
		return bbUploadedAttachment{}, err
	}

	// Uploads go to the web endpoint, not the REST one: /rest/api/1.0/.../
	// attachments only serves and deletes a single attachment by id, and answers
	// a POST to the collection with 405. Downloads still use REST (getAttachment).
	apiPath := c.rp(projectKey, repoSlug) + "/attachments"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+apiPath, &buf)
	if err != nil {
		return bbUploadedAttachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check") // XSRF guard for multipart uploads
	res, err := httpClient.Do(req)
	if err != nil {
		return bbUploadedAttachment{}, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return bbUploadedAttachment{}, fmt.Errorf("%s", formatBitbucketError(res.StatusCode, "POST", apiPath, parseBitbucketErrorDetails(string(raw))))
	}
	return parseUploadedAttachment(raw, src)
}

// parseUploadedAttachment reads the upload response, which wraps the file in an
// "attachments" array on current versions and returns it bare on older ones.
func parseUploadedAttachment(raw []byte, src resolvedAttachment) (bbUploadedAttachment, error) {
	var wrapped struct {
		Attachments []bbUploadedAttachment `json:"attachments"`
	}
	a := bbUploadedAttachment{}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Attachments) > 0 {
		a = wrapped.Attachments[0]
	} else if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("cannot read attachment upload response for %s: %w", src.name, err)
	}
	if a.ID.String() == "" && a.URL == "" {
		return a, fmt.Errorf("attachment upload for %s returned no id", src.name)
	}
	if a.Name == "" {
		a.Name = src.name
	}
	a.ref = src.ref
	return a, nil
}

// attachToText uploads the sources and returns text with each source reference
// that appears in it swapped for the attachment markup. Anything not referenced
// is appended, so a file passed without a mention still shows up.
func (c *BitbucketClient) attachToText(projectKey, repoSlug, text string, sources []string) (string, []bbUploadedAttachment, error) {
	uploaded, err := c.uploadAttachments(projectKey, repoSlug, sources)
	if err != nil {
		return text, nil, err
	}
	var extra []string
	for _, a := range uploaded {
		var spliced bool
		text, spliced = spliceAttachmentRef(text, a)
		if !spliced {
			extra = append(extra, a.markup())
		}
	}
	if len(extra) > 0 {
		text = strings.TrimRight(text, "\n")
		if text != "" {
			text += "\n\n"
		}
		text += strings.Join(extra, "\n")
	}
	return text, uploaded, nil
}

// spliceAttachmentRef rewrites a source reference already in the text: inside a
// markdown link target only the target is swapped (the caption survives),
// otherwise the bare reference becomes the full markup.
func spliceAttachmentRef(text string, a bbUploadedAttachment) (string, bool) {
	for _, cand := range refCandidates(a.ref) {
		if target := "](" + cand + ")"; strings.Contains(text, target) {
			return strings.ReplaceAll(text, target, "]("+a.link()+")"), true
		}
		if strings.Contains(text, cand) {
			return strings.ReplaceAll(text, cand, a.markup()), true
		}
	}
	return text, false
}

// refCandidates lists the spellings of a source the text may use, longest first
// so a relative path is never matched inside the absolute one. A URL has only
// the one spelling; inlined bytes have none, so they are always appended.
func refCandidates(ref string) []string {
	if ref == "" {
		return nil
	}
	out := []string{ref}
	if !hasHTTPScheme(ref) {
		if abs, err := filepath.Abs(ref); err == nil {
			if abs != ref {
				out = append(out, abs)
			}
			if cwd, err := os.Getwd(); err == nil {
				if rel, err := filepath.Rel(cwd, abs); err == nil && rel != abs && !strings.HasPrefix(rel, "..") {
					out = append(out, rel)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// attachmentSummary lists what was uploaded and the markup for it, so a later
// call can reference a file this one uploaded.
func attachmentSummary(uploaded []bbUploadedAttachment) string {
	if len(uploaded) == 0 {
		return ""
	}
	lines := make([]string, 0, len(uploaded))
	for _, a := range uploaded {
		lines = append(lines, fmt.Sprintf("  #%s %s — %s", a.ID.String(), a.Name, a.markup()))
	}
	return fmt.Sprintf("\nUploaded %d attachment(s):\n%s", len(uploaded), strings.Join(lines, "\n"))
}
