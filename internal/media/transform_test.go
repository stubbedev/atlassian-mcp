package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func markedImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), A: 255})
		}
	}
	return img
}

func at(img image.Image, x, y int) (int, int) {
	c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	return int(c.R), int(c.G)
}

func TestOrientFollowsEXIF(t *testing.T) {
	const w, h = 4, 3
	for _, tc := range []struct {
		o                  int
		origin, nextColumn image.Point
	}{
		{1, image.Pt(0, 0), image.Pt(1, 0)},
		{2, image.Pt(3, 0), image.Pt(2, 0)},
		{3, image.Pt(3, 2), image.Pt(2, 2)},
		{4, image.Pt(0, 2), image.Pt(1, 2)},
		{5, image.Pt(0, 0), image.Pt(0, 1)},
		{6, image.Pt(2, 0), image.Pt(2, 1)},
		{7, image.Pt(2, 3), image.Pt(2, 2)},
		{8, image.Pt(0, 3), image.Pt(0, 2)},
	} {
		out := orient(markedImage(w, h), tc.o)
		wantW, wantH := w, h
		if tc.o >= 5 {
			wantW, wantH = h, w
		}
		if b := out.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
			t.Errorf("orientation %d: size %v, want %dx%d", tc.o, b.Size(), wantW, wantH)
			continue
		}
		if x, y := at(out, tc.origin.X, tc.origin.Y); x != 0 || y != 0 {
			t.Errorf("orientation %d: stored (0,0) not at %v, found stored (%d,%d)", tc.o, tc.origin, x, y)
		}
		if x, y := at(out, tc.nextColumn.X, tc.nextColumn.Y); x != 1 || y != 0 {
			t.Errorf("orientation %d: stored (1,0) not at %v, found stored (%d,%d)", tc.o, tc.nextColumn, x, y)
		}
	}
}

func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	var tiff bytes.Buffer
	tiff.WriteString("MM\x00*")
	_ = binary.Write(&tiff, binary.BigEndian, uint32(8))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(1))
	_ = binary.Write(&tiff, binary.BigEndian, []uint16{0x0112, 3})
	_ = binary.Write(&tiff, binary.BigEndian, uint32(1))
	_ = binary.Write(&tiff, binary.BigEndian, []uint16{orientation, 0})
	_ = binary.Write(&tiff, binary.BigEndian, uint32(0))
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(plain.Bytes()[:2])
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(plain.Bytes()[2:])
	return out.Bytes()
}

func TestProcessImageAppliesEXIFRotation(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			c := color.NRGBA{R: 255, A: 255}
			if x >= 20 {
				c = color.NRGBA{B: 255, A: 255}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	data, mime, _, err := ProcessImage(jpegWithOrientation(t, src, 6), "image/jpeg", 1568, 90)
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %s", mime)
	}
	out, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := out.Bounds(); b.Dx() != 20 || b.Dy() != 40 {
		t.Fatalf("rotated size = %v, want 20x40", b.Size())
	}
	top := color.NRGBAModel.Convert(out.At(10, 5)).(color.NRGBA)
	bottom := color.NRGBAModel.Convert(out.At(10, 35)).(color.NRGBA)
	if top.R < 200 || top.B > 60 || bottom.B < 200 || bottom.R > 60 {
		t.Errorf("rotating clockwise should put the red left half on top: top %v, bottom %v", top, bottom)
	}
}

func TestFitKeepsAspectAndSmallImages(t *testing.T) {
	small := markedImage(10, 5)
	if fit(small, 100) != image.Image(small) {
		t.Error("an image within bounds should be returned as is")
	}
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{400, 100, 100, 25},
		{100, 400, 25, 100},
		{1000, 1, 100, 1},
	} {
		if b := fit(image.NewNRGBA(image.Rect(0, 0, tc.w, tc.h)), 100).Bounds(); b.Dx() != tc.wantW || b.Dy() != tc.wantH {
			t.Errorf("fit %dx%d = %v, want %dx%d", tc.w, tc.h, b.Size(), tc.wantW, tc.wantH)
		}
	}
}

func TestEXIFOrientationIgnoresMalformedHeaders(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("\xFF\xD8"), []byte("\xFF\xD8\xFF\xE1\x00\x01"), []byte("II*\x00\xFF\xFF\xFF\xFF")} {
		if o := exifOrientation(b); o != orientNormal {
			t.Errorf("exifOrientation(%q) = %d, want normal", b, o)
		}
	}
}
