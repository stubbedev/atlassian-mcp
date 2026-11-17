package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"

	xdraw "golang.org/x/image/draw"
)

const (
	orientNormal     = 1
	orientRotate180  = 3
	orientRotate90CW = 6
	orientRotate90CC = 8
)

func decodeOriented(buffer []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(buffer))
	if err != nil {
		return nil, err
	}
	return orient(img, exifOrientation(buffer)), nil
}

func exifOrientation(b []byte) int {
	if tiff := exifTIFF(b); tiff != nil {
		return tiffOrientation(tiff)
	}
	return tiffOrientation(b)
}

func exifTIFF(b []byte) []byte {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return nil
		}
		marker := b[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return nil
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		end := i + 2 + size
		if size < 2 || end > len(b) {
			return nil
		}
		seg := b[i+4 : end]
		if marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return seg[6:]
		}
		i = end
	}
	return nil
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return orientNormal
	}
	var order binary.ByteOrder
	switch string(t[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return orientNormal
	}
	ifd := int(order.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return orientNormal
	}
	entries := int(order.Uint16(t[ifd:]))
	for e := range entries {
		at := ifd + 2 + e*12
		if at+12 > len(t) {
			return orientNormal
		}
		if order.Uint16(t[at:]) == 0x0112 {
			if o := int(order.Uint16(t[at+8:])); o >= 1 && o <= 8 {
				return o
			}
			return orientNormal
		}
	}
	return orientNormal
}

func orient(img image.Image, o int) image.Image {
	if o <= orientNormal || o > 8 {
		return img
	}
	src := toNRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			sx, sy := orientSource(o, x, y, w, h)
			copy(dst.Pix[dst.PixOffset(x, y):][:4], src.Pix[src.PixOffset(sx, sy):][:4])
		}
	}
	return dst
}

func orientSource(o, x, y, w, h int) (int, int) {
	switch o {
	case 2:
		return w - 1 - x, y
	case 3:
		return w - 1 - x, h - 1 - y
	case 4:
		return x, h - 1 - y
	case 5:
		return y, x
	case 6:
		return y, h - 1 - x
	case 7:
		return w - 1 - y, h - 1 - x
	default:
		return w - 1 - y, x
	}
}

func rotateClockwise(img image.Image, degrees int) image.Image {
	switch degrees {
	case 90:
		return orient(img, orientRotate90CW)
	case 180:
		return orient(img, orientRotate180)
	case 270:
		return orient(img, orientRotate90CC)
	}
	return img
}

func toNRGBA(img image.Image) *image.NRGBA {
	b := img.Bounds()
	if n, ok := img.(*image.NRGBA); ok && b.Min == (image.Point{}) {
		return n
	}
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.Draw(dst, dst.Rect, img, b.Min, xdraw.Src)
	return dst
}

func fit(img image.Image, maxDim int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return img
	}
	dw, dh := maxDim, max(1, (h*maxDim+w/2)/w)
	if h > w {
		dw, dh = max(1, (w*maxDim+h/2)/h), maxDim
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	xdraw.CatmullRom.Scale(dst, dst.Rect, img, b, xdraw.Src, nil)
	return dst
}

// flattenOnWhite composites a page onto white paper: a page without a
// background renders transparent, which JPEG would turn black.
func flattenOnWhite(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.Draw(dst, dst.Rect, image.NewUniform(color.White), image.Point{}, xdraw.Src)
	xdraw.Draw(dst, dst.Rect, img, b.Min, xdraw.Over)
	return dst
}

func encodeJPEG(w io.Writer, img image.Image, quality int) error {
	return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
}

func encodePNG(w io.Writer, img image.Image) error {
	return (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(w, img)
}
