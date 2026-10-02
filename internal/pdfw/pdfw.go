// Package pdfw writes a PDF whose pages are JPEG images, which is what a scan is.
//
// The JPEG data is embedded as it is (the PDF DCTDecode filter), so nothing is
// re-compressed and no image library is needed beyond reading the picture size.
package pdfw

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"image/jpeg"
	"io"
	"time"
	"unicode/utf16"
)

// Page is one page of the document.
type Page struct {
	// JPEG is a complete JPEG file.
	JPEG []byte
	// DPI is the resolution the picture was scanned at; it sets the page size.
	DPI int
}

// Info is the document information shown by PDF viewers.
type Info struct {
	Title    string
	Author   string
	Creator  string
	Producer string
	Created  time.Time
}

type counter struct {
	w *bufio.Writer
	n int64
}

func (c *counter) printf(format string, args ...any) {
	n, _ := fmt.Fprintf(c.w, format, args...)
	c.n += int64(n)
}

func (c *counter) write(b []byte) {
	n, _ := c.w.Write(b)
	c.n += int64(n)
}

// Write writes the document.
func Write(out io.Writer, pages []Page, info Info) error {
	if len(pages) == 0 {
		return errors.New("pdfw: no pages")
	}
	type dims struct {
		w, h   int
		space  string
		decode string
	}
	sizes := make([]dims, len(pages))
	for i, p := range pages {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(p.JPEG))
		if err != nil {
			return fmt.Errorf("pdfw: page %d is not a JPEG: %w", i+1, err)
		}
		d := dims{w: cfg.Width, h: cfg.Height, space: "/DeviceRGB"}
		switch cfg.ColorModel {
		case color.GrayModel:
			d.space = "/DeviceGray"
		case color.CMYKModel:
			// Adobe CMYK JPEGs store inverted values.
			d.space, d.decode = "/DeviceCMYK", " /Decode [1 0 1 0 1 0 1 0]"
		}
		sizes[i] = d
	}

	c := &counter{w: bufio.NewWriterSize(out, 256<<10)}
	// Objects: 1 catalog, 2 page tree, 3 info, then per page: page, content, image.
	total := 3 + 3*len(pages)
	offsets := make([]int64, total+1)
	begin := func(n int) {
		offsets[n] = c.n
		c.printf("%d 0 obj\n", n)
	}

	c.write([]byte("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n"))
	begin(1)
	c.printf("<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	begin(2)
	c.printf("<< /Type /Pages /Count %d /Kids [", len(pages))
	for i := range pages {
		c.printf("%d 0 R ", 4+3*i)
	}
	c.printf("] >>\nendobj\n")
	begin(3)
	if info.Created.IsZero() {
		info.Created = time.Now()
	}
	if info.Producer == "" {
		info.Producer = "Platen"
	}
	c.printf("<< /Producer %s /CreationDate (D:%s)", pdfString(info.Producer), info.Created.UTC().Format("20060102150405Z"))
	if info.Title != "" {
		c.printf(" /Title %s", pdfString(info.Title))
	}
	if info.Author != "" {
		c.printf(" /Author %s", pdfString(info.Author))
	}
	if info.Creator != "" {
		c.printf(" /Creator %s", pdfString(info.Creator))
	}
	c.printf(" >>\nendobj\n")

	for i, p := range pages {
		d := sizes[i]
		dpi := p.DPI
		if dpi <= 0 {
			dpi = 300
		}
		wPt, hPt := float64(d.w)*72/float64(dpi), float64(d.h)*72/float64(dpi)
		pageObj, contentObj, imageObj := 4+3*i, 5+3*i, 6+3*i

		begin(pageObj)
		c.printf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Contents %d 0 R /Resources << /XObject << /Im0 %d 0 R >> >> >>\nendobj\n",
			wPt, hPt, contentObj, imageObj)

		content := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q\n", wPt, hPt)
		begin(contentObj)
		c.printf("<< /Length %d >>\nstream\n%sendstream\nendobj\n", len(content), content)

		begin(imageObj)
		c.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode%s /Length %d >>\nstream\n",
			d.w, d.h, d.space, d.decode, len(p.JPEG))
		c.write(p.JPEG)
		c.printf("\nendstream\nendobj\n")
	}

	xref := c.n
	c.printf("xref\n0 %d\n0000000000 65535 f \n", total+1)
	for n := 1; n <= total; n++ {
		c.printf("%010d 00000 n \n", offsets[n])
	}
	c.printf("trailer\n<< /Size %d /Root 1 0 R /Info 3 0 R >>\nstartxref\n%d\n%%%%EOF\n", total+1, xref)
	return c.w.Flush()
}

// pdfString encodes text as a PDF string: a literal for printable ASCII,
// UTF-16 in hex otherwise.
func pdfString(s string) string {
	ascii := true
	for _, r := range s {
		if r < 0x20 || r > 0x7E {
			ascii = false
			break
		}
	}
	if ascii {
		var b bytes.Buffer
		b.WriteByte('(')
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '(', ')', '\\':
				b.WriteByte('\\')
			}
			b.WriteByte(s[i])
		}
		b.WriteByte(')')
		return b.String()
	}
	var b bytes.Buffer
	b.WriteString("<FEFF")
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, "%04X", u)
	}
	b.WriteByte('>')
	return b.String()
}
