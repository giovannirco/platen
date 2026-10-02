package ipp

import (
	"strconv"
	"strings"
)

// Media is a paper size, parsed from a PWG self-describing media name such as
// iso_a4_210x297mm or na_letter_8.5x11in (PWG 5101.1).
type Media struct {
	Name string `json:"name"`
	// Width and Height are in hundredths of a millimetre, as IPP uses them.
	Width  int `json:"width_hmm"`
	Height int `json:"height_hmm"`
}

// ParseMedia reads the size out of a PWG media name.
func ParseMedia(name string) (Media, bool) {
	i := strings.LastIndex(name, "_")
	if i < 0 {
		return Media{}, false
	}
	dims := name[i+1:]
	var perUnit float64
	switch {
	case strings.HasSuffix(dims, "mm"):
		perUnit, dims = 100, strings.TrimSuffix(dims, "mm")
	case strings.HasSuffix(dims, "in"):
		perUnit, dims = 2540, strings.TrimSuffix(dims, "in")
	default:
		return Media{}, false
	}
	ws, hs, ok := strings.Cut(dims, "x")
	if !ok {
		return Media{}, false
	}
	w, err1 := strconv.ParseFloat(ws, 64)
	h, err2 := strconv.ParseFloat(hs, 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return Media{}, false
	}
	return Media{Name: name, Width: int(w*perUnit + 0.5), Height: int(h*perUnit + 0.5)}, true
}

// Points returns the size in PostScript points (1/72 inch), truncated like CUPS does.
func (m Media) Points() (w, h int) {
	return 72 * m.Width / 2540, 72 * m.Height / 2540
}

// Pixels returns the size in pixels at the given resolution, truncated like CUPS does.
func (m Media) Pixels(dpi int) (w, h int) {
	return m.Width * dpi / 2540, m.Height * dpi / 2540
}

// Label returns a short human name: "A4", "Letter", "4x6in".
func (m Media) Label() string {
	parts := strings.Split(m.Name, "_")
	if len(parts) < 3 {
		return m.Name
	}
	name := parts[1]
	switch {
	case parts[0] == "iso" && len(name) <= 3:
		return strings.ToUpper(name)
	case name == "letter", name == "legal", name == "executive", name == "ledger":
		return strings.ToUpper(name[:1]) + name[1:]
	case parts[0] == "jis":
		return "JIS " + strings.ToUpper(name)
	}
	return parts[len(parts)-1]
}
