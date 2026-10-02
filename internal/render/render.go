// Package render turns documents into page images ready for a printer.
//
// PDFs are rendered with PDFium compiled to WebAssembly and run inside the
// process by wazero, so Platen needs no C libraries, no Ghostscript and no CUPS.
// Images are scaled to the paper, and plain text is laid out with an embedded font.
package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	xdraw "golang.org/x/image/draw"
)

// Engine renders PDF pages. It starts the WebAssembly runtime on first use and
// renders one page at a time, which keeps memory use predictable.
type Engine struct {
	mu   sync.Mutex
	pool pdfium.Pool
	err  error
	init sync.Once
}

// NewEngine returns an Engine. Nothing is loaded until the first PDF is opened.
func NewEngine() *Engine { return &Engine{} }

func (e *Engine) start() error {
	e.init.Do(func() {
		e.pool, e.err = webassembly.Init(webassembly.Config{MinIdle: 1, MaxIdle: 1, MaxTotal: 1})
	})
	return e.err
}

// Close stops the WebAssembly runtime.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pool != nil {
		err := e.pool.Close()
		e.pool = nil
		return err
	}
	return nil
}

// PDF is an open PDF document. It holds the engine until Close is called.
type PDF struct {
	engine   *Engine
	instance pdfium.Pdfium
	doc      references.FPDF_DOCUMENT
	pages    int
	closed   bool
}

// ErrPassword is returned for PDFs that need a password.
var ErrPassword = errors.New("the PDF is password protected")

// OpenPDF opens a PDF held in memory. Only one PDF can be open at a time; a
// second call waits until the first document is closed.
func (e *Engine) OpenPDF(ctx context.Context, data []byte) (*PDF, error) {
	if err := e.start(); err != nil {
		return nil, fmt.Errorf("start PDF renderer: %w", err)
	}
	e.mu.Lock()
	ok := false
	defer func() {
		if !ok {
			e.mu.Unlock()
		}
	}()
	instCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	inst, err := e.pool.GetInstanceWithContext(instCtx)
	if err != nil {
		return nil, fmt.Errorf("PDF renderer busy: %w", err)
	}
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		_ = inst.Close()
		if isPasswordError(err) {
			return nil, ErrPassword
		}
		return nil, fmt.Errorf("open PDF: %w", err)
	}
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
		_ = inst.Close()
		return nil, fmt.Errorf("count PDF pages: %w", err)
	}
	ok = true
	return &PDF{engine: e, instance: inst, doc: doc.Document, pages: count.PageCount}, nil
}

func isPasswordError(err error) bool {
	return err != nil && (containsFold(err.Error(), "password") || containsFold(err.Error(), "FPDF_ERR_PASSWORD"))
}

// Pages returns the number of pages.
func (p *PDF) Pages() int { return p.pages }

// Close releases the document and the engine.
func (p *PDF) Close() {
	if p.closed {
		return
	}
	p.closed = true
	_, _ = p.instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: p.doc})
	_ = p.instance.Close()
	p.engine.mu.Unlock()
}

// PageSize returns the size of a page in points (1/72 inch).
func (p *PDF) PageSize(index int) (w, h float64, err error) {
	size, err := p.instance.GetPageSize(&requests.GetPageSize{Page: p.page(index)})
	if err != nil {
		return 0, 0, err
	}
	return size.Width, size.Height, nil
}

func (p *PDF) page(index int) requests.Page {
	return requests.Page{ByIndex: &requests.PageByIndex{Document: p.doc, Index: index}}
}

// Target describes the sheet a page is rendered for.
type Target struct {
	// Width and Height are the sheet size in pixels at the render resolution.
	Width, Height int
	// Gray renders 8-bit gray instead of colour.
	Gray bool
	// Margin is kept blank on every side, in pixels. PDFs are rendered edge to
	// edge (the printer clips what it can't print); images and text use it.
	Margin int
	// Fill makes an image cover the sheet (cropping) instead of fitting inside it.
	Fill bool
}

// RenderPage renders one page onto a white sheet of the target size. The page is
// scaled to fit, centred, and turned by 90 degrees when that matches the sheet's
// orientation better.
func (p *PDF) RenderPage(index int, t Target) (image.Image, error) {
	if index < 0 || index >= p.pages {
		return nil, fmt.Errorf("page %d out of range (1-%d)", index+1, p.pages)
	}
	pw, ph, err := p.PageSize(index)
	if err != nil {
		return nil, fmt.Errorf("page %d size: %w", index+1, err)
	}
	landscapePage, landscapeSheet := pw > ph, t.Width > t.Height
	rotate := landscapePage != landscapeSheet && pw != ph
	boxW, boxH := t.Width, t.Height
	if rotate {
		boxW, boxH = t.Height, t.Width
	}
	format := requests.RenderImageFormatRGBA
	if t.Gray {
		format = requests.RenderImageFormatGrayscale
	}
	res, err := p.instance.RenderPageInPixels(&requests.RenderPageInPixels{
		Page: p.page(index), Width: boxW, Height: boxH, ImageFormat: format,
	})
	if err != nil {
		return nil, fmt.Errorf("render page %d: %w", index+1, err)
	}
	defer res.Cleanup()
	// The pixel buffer belongs to the WebAssembly memory and is only valid until
	// Cleanup, so the page is copied onto the sheet here.
	return compose(res.Result.RenderedImage, t, rotate), nil
}

// compose places src centred on a white sheet, optionally rotated by 90 degrees.
func compose(src image.Image, t Target, rotate bool) image.Image {
	sheet := newSheet(t)
	if rotate {
		src = rotate90(src, t.Gray)
	}
	sb := src.Bounds()
	off := image.Pt((t.Width-sb.Dx())/2, (t.Height-sb.Dy())/2)
	draw.Draw(sheet, sb.Sub(sb.Min).Add(off), src, sb.Min, draw.Over)
	return sheet
}

func newSheet(t Target) draw.Image {
	rect := image.Rect(0, 0, t.Width, t.Height)
	if t.Gray {
		img := image.NewGray(rect)
		for i := range img.Pix {
			img.Pix[i] = 0xFF
		}
		return img
	}
	img := image.NewRGBA(rect)
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	return img
}

// rotate90 turns an image clockwise by 90 degrees.
func rotate90(src image.Image, gray bool) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	rect := image.Rect(0, 0, h, w)
	if g, ok := src.(*image.Gray); ok {
		dst := image.NewGray(rect)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Pix[x*dst.Stride+(h-1-y)] = g.Pix[y*g.Stride+x]
			}
		}
		return dst
	}
	if r, ok := src.(*image.RGBA); ok && !gray {
		dst := image.NewRGBA(rect)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				copy(dst.Pix[x*dst.Stride+4*(h-1-y):], r.Pix[y*r.Stride+4*x:y*r.Stride+4*x+4])
			}
		}
		return dst
	}
	var dst draw.Image
	if gray {
		dst = image.NewGray(rect)
	} else {
		dst = image.NewRGBA(rect)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(h-1-y, x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// FitImage scales a picture onto a white sheet: inside the margins by default,
// or covering the whole sheet when t.Fill is set. Landscape pictures are turned
// to match a portrait sheet and the other way round.
func FitImage(src image.Image, t Target) image.Image {
	sb := src.Bounds()
	if (sb.Dx() > sb.Dy()) != (t.Width > t.Height) && sb.Dx() != sb.Dy() {
		src = rotate90(src, false)
		sb = src.Bounds()
	}
	areaW, areaH := t.Width-2*t.Margin, t.Height-2*t.Margin
	if t.Fill {
		areaW, areaH = t.Width, t.Height
	}
	sx, sy := float64(areaW)/float64(sb.Dx()), float64(areaH)/float64(sb.Dy())
	scale := min(sx, sy)
	if t.Fill {
		scale = max(sx, sy)
	}
	w, h := int(float64(sb.Dx())*scale+0.5), int(float64(sb.Dy())*scale+0.5)
	sheet := newSheet(t)
	dst := image.Rect(0, 0, w, h).Add(image.Pt((t.Width-w)/2, (t.Height-h)/2))
	xdraw.CatmullRom.Scale(sheet, dst, src, sb, xdraw.Over, nil)
	return sheet
}

// Thumbnail scales an image so that its longer side is at most size pixels.
func Thumbnail(src image.Image, size int) image.Image {
	b := src.Bounds()
	if b.Dx() <= size && b.Dy() <= size {
		return src
	}
	scale := float64(size) / float64(max(b.Dx(), b.Dy()))
	w, h := max(1, int(float64(b.Dx())*scale+0.5)), max(1, int(float64(b.Dy())*scale+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

func containsFold(s, sub string) bool {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		match := true
		for j := 0; j < n; j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
