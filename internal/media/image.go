package media

import (
	"bytes"
	"image"
	"image/gif"
	"strings"

	_ "github.com/gen2brain/avif"   // AVIF, via libavif compiled to WebAssembly
	_ "github.com/gen2brain/heic"   // HEIC/HEIF (iPhone photos), via libheif in WebAssembly
	_ "github.com/gen2brain/jpegxl" // JPEG XL, via libjxl in WebAssembly
	_ "golang.org/x/image/bmp"      // BMP
	_ "golang.org/x/image/tiff"     // TIFF
	_ "golang.org/x/image/webp"     // WebP
)

func imageHasAlpha(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	return false
}

// ProcessImage resizes (long edge → maxDim) and re-encodes: PNG for images with
// alpha, JPEG otherwise. SVG passes through untouched.
func ProcessImage(buffer []byte, mimeType string, maxDim, quality int) (data []byte, outMime string, resized bool, err error) {
	if strings.ToLower(mimeType) == "image/svg+xml" {
		return buffer, mimeType, false, nil
	}
	img, err := decodeOriented(buffer)
	if err != nil {
		return nil, "", false, err
	}
	b := img.Bounds()
	needsResize := max(b.Dx(), b.Dy()) > maxDim
	out := fit(img, maxDim)
	var buf bytes.Buffer
	if imageHasAlpha(img) {
		if err := encodePNG(&buf, out); err != nil {
			return nil, "", false, err
		}
		return buf.Bytes(), "image/png", needsResize, nil
	}
	if err := encodeJPEG(&buf, out, quality); err != nil {
		return nil, "", false, err
	}
	return buf.Bytes(), "image/jpeg", needsResize, nil
}

var pngSig = []byte{0x89, 0x50, 0x4E, 0x47}

func IsAnimatedImage(buffer []byte, mimeType string) bool {
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

func ReencodeFrame(frame []byte, maxDim, quality int) []byte {
	img, err := decodeOriented(frame)
	if err != nil {
		return frame
	}
	if out := EncodeFittedJPEG(img, maxDim, quality); out != nil {
		return out
	}
	return frame
}

// EncodeFittedJPEG shrinks img to fit maxDim and encodes it as JPEG; nil on failure.
func EncodeFittedJPEG(img image.Image, maxDim, quality int) []byte {
	var buf bytes.Buffer
	if encodeJPEG(&buf, fit(img, maxDim), quality) != nil {
		return nil
	}
	return buf.Bytes()
}

// ── PDF ──────────────────────────────────────────────────────────────────────
