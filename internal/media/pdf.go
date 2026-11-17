package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/kovidgoyal/imaging"
	"github.com/ledongthuc/pdf"
)

const PDFRasterThreshold = 20

const PDFMaxRasterPages = 3

func ExtractPDFText(buffer []byte) (totalPages int, body string, err error) {
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

// pdfiumPool is PDFium compiled to WebAssembly, run in-process by wazero.
// Starting it compiles the module, so it happens once, on first use.
var pdfiumPool = sync.OnceValues(func() (pdfium.Pool, error) {
	return webassembly.Init(webassembly.Config{MinIdle: 0, MaxIdle: 1, MaxTotal: 2})
})

// RasterizePDF renders the first pageCount pages to fit maxDim, the way a
// viewer would show them.
func RasterizePDF(buffer []byte, pageCount, maxDim int) ([]image.Image, error) {
	pool, err := pdfiumPool()
	if err != nil {
		return nil, fmt.Errorf("PDF renderer unavailable: %w", err)
	}
	inst, err := pool.GetInstance(time.Minute)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inst.Close() }()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &buffer})
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document}) }()
	var out []image.Image
	for i := range pageCount {
		res, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
			Page:   requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i}},
			Width:  maxDim,
			Height: maxDim,
		})
		if err != nil {
			return out, err
		}
		// The pixels live in the WebAssembly heap until Cleanup; copy them out.
		img := imaging.Clone(res.Result.RenderedImage)
		res.Cleanup()
		out = append(out, flattenOnWhite(img))
	}
	return out, nil
}

// flattenOnWhite composites a page onto white paper: a page without a
// background renders transparent, which JPEG would turn black.
func flattenOnWhite(img image.Image) image.Image {
	bg := imaging.New(img.Bounds().Dx(), img.Bounds().Dy(), color.White)
	return imaging.Overlay(bg, img, image.Point{}, 1)
}
