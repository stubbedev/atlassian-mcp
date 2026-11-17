package media

import (
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

const PDFRasterThreshold = 20

const PDFMaxRasterPages = 3

func ExtractPDFText(buffer []byte) (totalPages int, body string, err error) {
	err = withPDF(buffer, func(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, pages int) error {
		totalPages = pages
		texts := make([]string, 0, pages)
		for i := range pages {
			res, err := inst.GetPageText(&requests.GetPageText{Page: pageAt(doc, i)})
			if err != nil {
				return fmt.Errorf("reading text of page %d: %w", i+1, err)
			}
			texts = append(texts, res.Text)
		}
		body = strings.TrimSpace(strings.Join(texts, "\n\n"))
		return nil
	})
	return totalPages, body, err
}

// pdfiumPool is PDFium compiled to WebAssembly, run in-process by wazero.
// Starting it compiles the module, so it happens once, on first use.
var pdfiumPool = sync.OnceValues(func() (pdfium.Pool, error) {
	return webassembly.Init(webassembly.Config{MinIdle: 0, MaxIdle: 1, MaxTotal: 2})
})

func withPDF(buffer []byte, use func(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, pages int) error) error {
	pool, err := pdfiumPool()
	if err != nil {
		return fmt.Errorf("PDF engine unavailable: %w", err)
	}
	inst, err := pool.GetInstance(time.Minute)
	if err != nil {
		return fmt.Errorf("starting the PDF engine: %w", err)
	}
	defer func() { _ = inst.Close() }()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &buffer})
	if err != nil {
		return fmt.Errorf("opening the PDF: %w", err)
	}
	defer func() { _, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document}) }()
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return fmt.Errorf("counting PDF pages: %w", err)
	}
	return use(inst, doc.Document, count.PageCount)
}

func pageAt(doc references.FPDF_DOCUMENT, index int) requests.Page {
	return requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: index}}
}

// RasterizePDF renders the first pageCount pages to fit maxDim, the way a
// viewer would show them.
func RasterizePDF(buffer []byte, pageCount, maxDim int) ([]image.Image, error) {
	var out []image.Image
	err := withPDF(buffer, func(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, pages int) error {
		for i := range min(pageCount, pages) {
			res, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{Page: pageAt(doc, i), Width: maxDim, Height: maxDim})
			if err != nil {
				return fmt.Errorf("rendering page %d: %w", i+1, err)
			}
			// The pixels live in the WebAssembly heap until Cleanup; flattening copies them out.
			out = append(out, flattenOnWhite(res.Result.RenderedImage))
			res.Cleanup()
		}
		return nil
	})
	return out, err
}
