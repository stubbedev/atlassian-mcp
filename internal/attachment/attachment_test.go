package attachment

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stubbedev/atlassian-mcp/internal/config"
)

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{500, "500 B"},
		{2048, "2.0 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, c := range cases {
		if got := FormatBytes(c.in); got != c.want {
			t.Errorf("%d: got %q want %q", c.in, got, c.want)
		}
	}
}

// ── attachment sources ───────────────────────────────────────────────────────

func TestResolveAttachmentSourceLocalAndDataURI(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := resolveAttachmentSource(local, nil)
	if err != nil {
		t.Fatalf("local: %v", err)
	}
	defer Release([]Resolved{r})
	if r.Name != "notes.txt" || r.tmp {
		t.Errorf("local resolved = %+v", r)
	}

	// A data: URI carries its own bytes, so it works with no filesystem at all —
	// the transport a sandboxed host is left with.
	d, err := resolveAttachmentSource("data:text/plain;name=run.log;base64,"+base64.StdEncoding.EncodeToString([]byte("boom")), nil)
	if err != nil {
		t.Fatalf("data URI: %v", err)
	}
	defer Release([]Resolved{d})
	if d.Name != "run.log" || !d.tmp {
		t.Fatalf("data resolved = %+v", d)
	}
	got, err := os.ReadFile(d.Path)
	if err != nil || string(got) != "boom" {
		t.Errorf("data bytes = %q, %v", got, err)
	}

	// An unnamed data: URI gets an extension from its media type.
	png, err := resolveAttachmentSource("data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("\x89PNG")), nil)
	if err != nil {
		t.Fatalf("png data URI: %v", err)
	}
	defer Release([]Resolved{png})
	if png.Name != "attachment.png" {
		t.Errorf("png name = %q", png.Name)
	}

	// An unreadable path must name the alternatives that need no filesystem.
	_, err = resolveAttachmentSource(filepath.Join(dir, "missing.png"), nil)
	if err == nil || !strings.Contains(err.Error(), "data:") ||
		!strings.Contains(err.Error(), "confined") {
		t.Errorf("missing path error = %v", err)
	}
}

func TestResolveAttachmentSourceURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/named":
			w.Header().Set("Content-Disposition", `attachment; filename="diagram.png"`)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("bytes"))
		case "/1234": // Bitbucket-style: a bare id, type only in the header.
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("bytes"))
		case "/secret":
			if r.Header.Get("Authorization") != "Bearer jira-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("nope"))
		}
	}))
	defer srv.Close()

	r, err := resolveAttachmentSource(srv.URL+"/named", nil)
	if err != nil {
		t.Fatalf("named: %v", err)
	}
	defer Release([]Resolved{r})
	if r.Name != "diagram.png" || !r.tmp || r.Ref != srv.URL+"/named" {
		t.Errorf("named resolved = %+v", r)
	}

	bare, err := resolveAttachmentSource(srv.URL+"/1234", nil)
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	defer Release([]Resolved{bare})
	if bare.Name != "1234.jpg" {
		t.Errorf("bare name = %q, want an extension from Content-Type", bare.Name)
	}

	// A URL on the configured Jira host is fetched with the token, so an
	// attachment already in Jira can be re-attached elsewhere.
	auth, err := resolveAttachmentSource(srv.URL+"/secret", config.Credentials{{URL: srv.URL, Token: "jira-token"}})
	if err != nil {
		t.Fatalf("authenticated: %v", err)
	}
	defer Release([]Resolved{auth})

	// A host we are not configured for gets no token, and the failure says so.
	if _, err := resolveAttachmentSource(srv.URL+"/gone", nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing URL error = %v", err)
	}
}

// TestWidgetDataURIFormat pins the exact string the upload panel emits. The
// panel builds it in JavaScript and this parses it in Go, so nothing but a test
// holds the two halves together — a change to either side breaks here first.
func TestWidgetDataURIFormat(t *testing.T) {
	// Captured from the widget driven against a stub host: a 4-byte PNG picked
	// as "shot.png".
	const fromWidget = "data:image/png;name=shot.png;base64,AQIDBA=="

	r, err := resolveAttachmentSource(fromWidget, nil)
	if err != nil {
		t.Fatalf("widget data URI: %v", err)
	}
	defer Release([]Resolved{r})
	if r.Name != "shot.png" {
		t.Errorf("name = %q, want shot.png", r.Name)
	}
	got, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("bytes = %v, want %v", got, want)
	}
	// The panel names a pasted screenshot rather than sending an empty one.
	p, err := resolveAttachmentSource("data:image/png;name=pasted-image;base64,CQk=", nil)
	if err != nil {
		t.Fatalf("pasted image: %v", err)
	}
	defer Release([]Resolved{p})
	if p.Name != "pasted-image" {
		t.Errorf("pasted name = %q", p.Name)
	}
}
