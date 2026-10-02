package render

import (
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	monoOnce sync.Once
	monoFont *opentype.Font
	monoErr  error
)

func mono() (*opentype.Font, error) {
	monoOnce.Do(func() { monoFont, monoErr = opentype.Parse(gomono.TTF) })
	return monoFont, monoErr
}

// TextLayout holds plain text broken into pages for a given sheet.
type TextLayout struct {
	pages  [][]string
	target Target
	dpi    int
	points float64
}

// TextOptions tune how plain text is laid out.
type TextOptions struct {
	// Points is the font size. Defaults to 10.
	Points float64
	// TabWidth is the number of columns a tab advances to. Defaults to 4.
	TabWidth int
}

// LayoutText wraps and paginates text for a sheet. dpi is the render resolution
// that t.Width and t.Height are expressed in.
func LayoutText(text string, t Target, dpi int, opts TextOptions) (*TextLayout, error) {
	if opts.Points <= 0 {
		opts.Points = 10
	}
	if opts.TabWidth <= 0 {
		opts.TabWidth = 4
	}
	face, err := newFace(opts.Points, dpi)
	if err != nil {
		return nil, err
	}
	defer face.Close()
	advance, ok := face.GlyphAdvance('M')
	if !ok || advance <= 0 {
		return nil, fmt.Errorf("font has no advance for 'M'")
	}
	lineHeight := face.Metrics().Height.Ceil()
	cols := (t.Width - 2*t.Margin) * 64 / int(advance)
	rows := (t.Height - 2*t.Margin) / lineHeight
	if cols < 10 || rows < 5 {
		return nil, fmt.Errorf("sheet too small for text at %g pt", opts.Points)
	}

	var lines []string
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, raw := range strings.Split(text, "\n") {
		if raw == "\f" {
			lines = append(lines, "\f")
			continue
		}
		line := expandTabs(strings.ToValidUTF8(raw, "?"), opts.TabWidth)
		for utf8.RuneCountInString(line) > cols {
			cut := wrapAt(line, cols)
			lines = append(lines, strings.TrimRight(line[:cut], " "))
			line = strings.TrimLeft(line[cut:], " ")
		}
		lines = append(lines, line)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	layout := &TextLayout{target: t, dpi: dpi, points: opts.Points}
	var page []string
	flush := func() {
		layout.pages = append(layout.pages, page)
		page = nil
	}
	for _, l := range lines {
		if l == "\f" {
			flush()
			continue
		}
		page = append(page, l)
		if len(page) == rows {
			flush()
		}
	}
	if len(page) > 0 || len(layout.pages) == 0 {
		flush()
	}
	return layout, nil
}

// Pages returns the number of pages.
func (l *TextLayout) Pages() int { return len(l.pages) }

// RenderPage draws one page of text on a white sheet.
func (l *TextLayout) RenderPage(index int) (image.Image, error) {
	if index < 0 || index >= len(l.pages) {
		return nil, fmt.Errorf("page %d out of range (1-%d)", index+1, len(l.pages))
	}
	face, err := newFace(l.points, l.dpi)
	if err != nil {
		return nil, err
	}
	defer face.Close()
	sheet := newSheet(l.target)
	m := face.Metrics()
	d := font.Drawer{Dst: sheet, Src: image.NewUniform(color.Black), Face: face}
	y := l.target.Margin + m.Ascent.Ceil()
	for _, line := range l.pages[index] {
		d.Dot = fixed.P(l.target.Margin, y)
		d.DrawString(line)
		y += m.Height.Ceil()
	}
	return sheet, nil
}

func newFace(points float64, dpi int) (font.Face, error) {
	f, err := mono()
	if err != nil {
		return nil, fmt.Errorf("load font: %w", err)
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: points, DPI: float64(dpi), Hinting: font.HintingFull})
}

func expandTabs(s string, width int) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := width - col%width
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

// wrapAt returns the byte offset at which to break a line that is longer than
// cols runes: at the last space before the limit, or at the limit itself.
func wrapAt(line string, cols int) int {
	limit, lastSpace, n := len(line), -1, 0
	for i, r := range line {
		if n == cols {
			limit = i
			break
		}
		if r == ' ' {
			lastSpace = i
		}
		n++
	}
	if lastSpace > limit/2 {
		return lastSpace
	}
	return limit
}
