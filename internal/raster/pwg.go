// Package raster writes PWG Raster (image/pwg-raster), the page format that every
// IPP Everywhere, AirPrint and Mopria printer accepts.
//
// The format is specified in PWG 5102.4 "PWG Raster Format". A file is the
// synchronization word "RaS2" followed by pages; each page is a 1796-byte header
// and run-length encoded pixel rows.
package raster

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
)

// HeaderSize is the size of a page header in bytes.
const HeaderSize = 1796

// Colour space numbers used in the header (CUPS raster values).
const (
	colorSpaceSGray = 18
	colorSpaceSRGB  = 19
)

// SheetBack says how the printer wants the back side of a duplex sheet delivered
// (the pwg-raster-document-sheet-back printer attribute).
type SheetBack string

// Values of pwg-raster-document-sheet-back.
const (
	BackNormal       SheetBack = "normal"
	BackFlipped      SheetBack = "flipped"
	BackRotated      SheetBack = "rotated"
	BackManualTumble SheetBack = "manual-tumble"
)

// Page describes one page to write.
type Page struct {
	// Image holds the pixels. *image.RGBA and *image.Gray are read directly;
	// other types go through the generic color model.
	Image image.Image
	// Scale repeats every source pixel Scale times in both directions. It lets a
	// page rendered at 300 dpi be sent to a printer that only takes 600 dpi.
	Scale int
	// DPI is the resolution of the written page (after scaling).
	DPI int
	// Gray writes 8-bit gray (sgray_8) instead of 24-bit colour (srgb_8).
	Gray bool
	// MediaName is the PWG media name, e.g. iso_a4_210x297mm.
	MediaName string
	// PageSizePoints is the media size in points.
	PageSizePoints [2]int
	// Duplex and Tumble describe two-sided printing (tumble = short-edge binding).
	Duplex, Tumble bool
	// Back marks the back side of a duplex sheet. Together with SheetBack it
	// decides whether the page must be rotated or mirrored.
	Back      bool
	SheetBack SheetBack
	// TotalPages is the number of pages in the document, or 0 when unknown.
	TotalPages int
	// Quality is the IPP print-quality value (3 draft, 4 normal, 5 high) or 0.
	Quality int
}

// Writer writes pages to a PWG Raster stream.
type Writer struct {
	w       *bufio.Writer
	started bool
	row     []byte
	next    []byte
}

// NewWriter returns a Writer. Call Flush when done.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 256<<10)}
}

// Flush writes buffered data to the underlying writer.
func (w *Writer) Flush() error { return w.w.Flush() }

// transform returns how the image must be turned for this page: flip columns
// (cross-feed direction), flip rows (feed direction).
func (p Page) transform() (flipX, flipY bool) {
	if !p.Duplex || !p.Back {
		return false, false
	}
	switch p.SheetBack {
	case BackFlipped:
		// Mirrored along the binding edge.
		if p.Tumble {
			return true, false
		}
		return false, true
	case BackRotated:
		// Rotated 180 degrees for long-edge binding.
		if !p.Tumble {
			return true, true
		}
	case BackManualTumble:
		// Rotated 180 degrees for short-edge binding.
		if p.Tumble {
			return true, true
		}
	}
	return false, false
}

// WritePage encodes one page.
func (w *Writer) WritePage(p Page) error {
	if p.Image == nil {
		return errors.New("raster: page has no image")
	}
	if p.Scale < 1 {
		p.Scale = 1
	}
	b := p.Image.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return errors.New("raster: empty page image")
	}
	width, height := srcW*p.Scale, srcH*p.Scale
	bpp := 3
	if p.Gray {
		bpp = 1
	}
	if !w.started {
		if _, err := w.w.WriteString("RaS2"); err != nil {
			return err
		}
		w.started = true
	}
	flipX, flipY := p.transform()
	if err := w.writeHeader(p, width, height, bpp, flipX, flipY); err != nil {
		return err
	}

	rowLen := width * bpp
	if cap(w.row) < rowLen {
		w.row, w.next = make([]byte, rowLen), make([]byte, rowLen)
	}
	row, next := w.row[:rowLen], w.next[:rowLen]
	read := newRowReader(p.Image, p.Gray, p.Scale, flipX)
	srcRow := func(i int) int {
		if flipY {
			return srcH - 1 - i
		}
		return i
	}

	// Identical consecutive rows are written once with a repeat count. Scaling
	// makes every source row count Scale times.
	read(srcRow(0), row)
	for y := 0; y < srcH; {
		same, differs := 1, false
		for y+same < srcH {
			read(srcRow(y+same), next)
			if !bytes.Equal(row, next) {
				differs = true
				break
			}
			same++
		}
		for repeat := same * p.Scale; repeat > 0; {
			n := min(repeat, 256)
			if err := w.w.WriteByte(byte(n - 1)); err != nil {
				return err
			}
			if err := encodeRow(w.w, row, bpp); err != nil {
				return err
			}
			repeat -= n
		}
		y += same
		if differs {
			row, next = next, row
		}
	}
	return nil
}

func (w *Writer) writeHeader(p Page, width, height, bpp int, flipX, flipY bool) error {
	var h [HeaderSize]byte
	put := func(off int, v uint32) { binary.BigEndian.PutUint32(h[off:], v) }
	copy(h[0:64], "PwgRaster")
	if p.Duplex {
		put(272, 1)
	}
	put(276, uint32(p.DPI)) // HWResolution
	put(280, uint32(p.DPI))
	put(352, uint32(p.PageSizePoints[0]))
	put(356, uint32(p.PageSizePoints[1]))
	if p.Tumble {
		put(368, 1)
	}
	put(372, uint32(width))
	put(376, uint32(height))
	put(384, 8)                 // cupsBitsPerColor
	put(388, uint32(8*bpp))     // cupsBitsPerPixel
	put(392, uint32(width*bpp)) // cupsBytesPerLine
	put(396, 0)                 // cupsColorOrder: chunky
	if p.Gray {
		put(400, colorSpaceSGray)
		put(420, 1) // cupsNumColors
	} else {
		put(400, colorSpaceSRGB)
		put(420, 3)
	}
	put(452, uint32(p.TotalPages)) // TotalPageCount
	cross, feed := int32(1), int32(1)
	if flipX {
		cross = -1
	}
	if flipY {
		feed = -1
	}
	put(456, uint32(cross)) // CrossFeedTransform
	put(460, uint32(feed))  // FeedTransform
	put(464, 0)             // ImageBoxLeft
	put(468, 0)             // ImageBoxTop
	put(472, uint32(width)) // ImageBoxRight
	put(476, uint32(height))
	put(480, 0x00FFFFFF) // AlternatePrimary: white
	put(484, uint32(p.Quality))
	copy(h[1732:1796], p.MediaName)
	_, err := w.w.Write(h[:])
	return err
}

// encodeRow writes one row with the PackBits-style scheme of PWG Raster:
// a count byte 0..127 means "repeat the next pixel count+1 times";
// a count byte 129..255 means "257-count literal pixels follow".
func encodeRow(w *bufio.Writer, row []byte, bpp int) error {
	n := len(row) / bpp
	px := func(i int) []byte { return row[i*bpp : (i+1)*bpp] }
	for i := 0; i < n; {
		// Run of equal pixels.
		j := i + 1
		for j < n && j-i < 128 && bytes.Equal(px(j), px(i)) {
			j++
		}
		if j-i > 1 {
			if err := w.WriteByte(byte(j - i - 1)); err != nil {
				return err
			}
			if _, err := w.Write(px(i)); err != nil {
				return err
			}
			i = j
			continue
		}
		// Literal run: until two equal pixels in a row start, or 128 pixels.
		j = i + 1
		for j < n && j-i < 128 && !(j+1 < n && bytes.Equal(px(j), px(j+1))) {
			j++
		}
		if j-i == 1 {
			if err := w.WriteByte(0); err != nil {
				return err
			}
		} else if err := w.WriteByte(byte(257 - (j - i))); err != nil {
			return err
		}
		if _, err := w.Write(row[i*bpp : j*bpp]); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// newRowReader returns a function that fills dst with source row y, converted to
// packed sRGB or gray, each pixel repeated scale times, optionally mirrored.
func newRowReader(img image.Image, gray bool, scale int, flipX bool) func(y int, dst []byte) {
	b := img.Bounds()
	w := b.Dx()
	bpp := 3
	if gray {
		bpp = 1
	}
	// put writes one source pixel at column x (in source coordinates).
	place := func(dst []byte, x int, r, g, bl uint8) {
		if flipX {
			x = w - 1 - x
		}
		o := x * scale * bpp
		if gray {
			v := uint8((299*int(r) + 587*int(g) + 114*int(bl) + 500) / 1000)
			for s := 0; s < scale; s++ {
				dst[o+s] = v
			}
			return
		}
		for s := 0; s < scale; s++ {
			dst[o+3*s], dst[o+3*s+1], dst[o+3*s+2] = r, g, bl
		}
	}
	switch src := img.(type) {
	case *image.RGBA:
		return func(y int, dst []byte) {
			line := src.Pix[y*src.Stride : y*src.Stride+w*4]
			for x := 0; x < w; x++ {
				r, g, bl, a := line[4*x], line[4*x+1], line[4*x+2], line[4*x+3]
				if a != 0xFF { // premultiplied alpha over white paper
					r, g, bl = r+(0xFF-a), g+(0xFF-a), bl+(0xFF-a)
				}
				place(dst, x, r, g, bl)
			}
		}
	case *image.Gray:
		return func(y int, dst []byte) {
			line := src.Pix[y*src.Stride : y*src.Stride+w]
			for x := 0; x < w; x++ {
				place(dst, x, line[x], line[x], line[x])
			}
		}
	default:
		return func(y int, dst []byte) {
			for x := 0; x < w; x++ {
				r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				if a != 0xFFFF {
					r, g, bl = r+(0xFFFF-a), g+(0xFFFF-a), bl+(0xFFFF-a)
				}
				place(dst, x, uint8(r>>8), uint8(g>>8), uint8(bl>>8))
			}
		}
	}
}

// DecodedPage is a page read back from a PWG Raster stream.
type DecodedPage struct {
	Width, Height, DPI int
	Gray               bool
	Duplex, Tumble     bool
	MediaName          string
	PageSizePoints     [2]int
	TotalPages         int
	CrossFeed, Feed    int
	// Pix holds the rows, packed sRGB (3 bytes per pixel) or gray (1 byte).
	Pix []byte
}

// Decode reads every page of a PWG Raster stream. It exists for tests and for
// checking what Platen sends; it accepts only the 8-bit sRGB and sGray types
// that the Writer produces.
func Decode(r io.Reader) ([]DecodedPage, error) {
	br := bufio.NewReader(r)
	var sync [4]byte
	if _, err := io.ReadFull(br, sync[:]); err != nil {
		return nil, fmt.Errorf("raster: read sync word: %w", err)
	}
	if string(sync[:]) != "RaS2" {
		return nil, fmt.Errorf("raster: not a PWG Raster stream (sync %q)", sync[:])
	}
	var pages []DecodedPage
	for {
		var h [HeaderSize]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return pages, nil
			}
			return nil, fmt.Errorf("raster: read page header: %w", err)
		}
		get := func(off int) int { return int(binary.BigEndian.Uint32(h[off:])) }
		p := DecodedPage{
			Width: get(372), Height: get(376), DPI: get(276),
			Duplex: get(272) == 1, Tumble: get(368) == 1,
			PageSizePoints: [2]int{get(352), get(356)},
			TotalPages:     get(452),
			CrossFeed:      int(int32(binary.BigEndian.Uint32(h[456:]))),
			Feed:           int(int32(binary.BigEndian.Uint32(h[460:]))),
			MediaName:      string(bytes.TrimRight(h[1732:1796], "\x00")),
		}
		bpp := get(388) / 8
		switch get(400) {
		case colorSpaceSGray:
			p.Gray = true
		case colorSpaceSRGB:
		default:
			return nil, fmt.Errorf("raster: unsupported colour space %d", get(400))
		}
		if bpp != 1 && bpp != 3 || p.Width <= 0 || p.Height <= 0 || p.Width > 1<<16 || p.Height > 1<<17 {
			return nil, fmt.Errorf("raster: unsupported page geometry %dx%d, %d bytes per pixel", p.Width, p.Height, bpp)
		}
		rowLen := p.Width * bpp
		p.Pix = make([]byte, 0, rowLen*p.Height)
		row := make([]byte, rowLen)
		for y := 0; y < p.Height; {
			repeat, err := br.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("raster: page %d row %d: %w", len(pages)+1, y, err)
			}
			for x := 0; x < p.Width; {
				c, err := br.ReadByte()
				if err != nil {
					return nil, fmt.Errorf("raster: page %d row %d: %w", len(pages)+1, y, err)
				}
				switch {
				case c <= 127:
					n := int(c) + 1
					if x+n > p.Width {
						return nil, fmt.Errorf("raster: page %d row %d overflows", len(pages)+1, y)
					}
					if _, err := io.ReadFull(br, row[x*bpp:(x+1)*bpp]); err != nil {
						return nil, err
					}
					for i := 1; i < n; i++ {
						copy(row[(x+i)*bpp:(x+i+1)*bpp], row[x*bpp:(x+1)*bpp])
					}
					x += n
				case c == 128:
					return nil, fmt.Errorf("raster: page %d row %d: reserved count byte", len(pages)+1, y)
				default:
					n := 257 - int(c)
					if x+n > p.Width {
						return nil, fmt.Errorf("raster: page %d row %d overflows", len(pages)+1, y)
					}
					if _, err := io.ReadFull(br, row[x*bpp:(x+n)*bpp]); err != nil {
						return nil, err
					}
					x += n
				}
			}
			for i := 0; i <= int(repeat) && y < p.Height; i++ {
				p.Pix = append(p.Pix, row...)
				y++
			}
		}
		pages = append(pages, p)
	}
}
