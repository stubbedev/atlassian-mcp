package attachment

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stubbedev/atlassian-mcp/internal/media"
)

// minimalPDF is one page holding a filled black rectangle and no text, so
// it takes the scanned-PDF path: rasterize instead of extracting text.
func minimalPDF() []byte {
	return onePagePDF("0 0 0 rg 50 50 100 100 re f", "")
}

func textPDF(lines ...string) []byte {
	var content strings.Builder
	content.WriteString("BT /F1 18 Tf 20 160 Td 22 TL")
	for _, l := range lines {
		fmt.Fprintf(&content, " (%s) Tj T*", l)
	}
	content.WriteString(" ET")
	return onePagePDF(content.String(), "/Resources << /Font << /F1 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> >> >>")
}

func onePagePDF(content, resources string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R " + resources + " >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestPDFTextIsExtracted(t *testing.T) {
	res := buildPDFResult(Args{ID: "2", Filename: "notes.pdf", Buffer: textPDF("Release notes for 9.1", "Fixed the upload panel")}, "notes.pdf")
	if len(res.Content) != 1 {
		t.Fatalf("a text PDF should come back as one text block, got %d blocks", len(res.Content))
	}
	got := res.Content[0].Text
	for _, want := range []string{"Extracted text from 1 page(s):", "Release notes for 9.1", "Fixed the upload panel"} {
		if !strings.Contains(got, want) {
			t.Errorf("result missing %q:\n%s", want, got)
		}
	}
}

func TestBrokenPDFIsReported(t *testing.T) {
	res := buildPDFResult(Args{ID: "3", Filename: "broken.pdf", Buffer: []byte("%PDF-1.4\nnot really")}, "broken.pdf")
	if got := res.Content[0].Text; !strings.Contains(got, "Failed to extract PDF text") {
		t.Errorf("a broken PDF should say text extraction failed:\n%s", got)
	}
}

func TestRasterizePDFInProcess(t *testing.T) {
	imgs, err := media.RasterizePDF(minimalPDF(), 1, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("%d pages", len(imgs))
	}
	img := imgs[0]
	if b := img.Bounds(); b.Dx() != 400 || b.Dy() != 400 {
		t.Errorf("page size = %v", b)
	}
	// PDF y runs up: the rectangle spans 50–150 of 200, i.e. 100–300 px
	// either way, inside a white page.
	if r, _, _, _ := img.At(200, 200).RGBA(); r>>8 > 30 {
		t.Errorf("rectangle not drawn: %d", r>>8)
	}
	if r, _, _, _ := img.At(20, 20).RGBA(); r>>8 < 225 {
		t.Errorf("page not white: %d", r>>8)
	}

	res := buildPDFResult(Args{ID: "1", Filename: "scan.pdf", Buffer: minimalPDF()}, "scan.pdf")
	if len(res.Content) != 3 || res.Content[2].Type != "image" {
		t.Errorf("scanned PDF result: %+v", res.Content[0].Text)
	}
}
