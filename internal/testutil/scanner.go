package testutil

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
)

// Capabilities is a ScannerCapabilities document of a flatbed scanner, modelled
// on what a Canon PIXMA reports.
const Capabilities = `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScannerCapabilities xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
<pwg:Version>2.63</pwg:Version><pwg:MakeAndModel>Platen Test Scanner</pwg:MakeAndModel><pwg:SerialNumber>TEST0001</pwg:SerialNumber>
<scan:UUID>00000000-0000-1000-8000-000000000001</scan:UUID><scan:AdminURI>http://scanner.local./index.html</scan:AdminURI>
<scan:Platen><scan:PlatenInputCaps><scan:MinWidth>1</scan:MinWidth><scan:MaxWidth>2550</scan:MaxWidth><scan:MinHeight>1</scan:MinHeight><scan:MaxHeight>3508</scan:MaxHeight>
<scan:SettingProfiles><scan:SettingProfile>
<scan:ColorModes><scan:ColorMode>Grayscale8</scan:ColorMode><scan:ColorMode scan:default="true">RGB24</scan:ColorMode></scan:ColorModes>
<scan:DocumentFormats><pwg:DocumentFormat>image/jpeg</pwg:DocumentFormat><pwg:DocumentFormat>application/pdf</pwg:DocumentFormat><scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt></scan:DocumentFormats>
<scan:SupportedResolutions><scan:DiscreteResolutions>
<scan:DiscreteResolution><scan:XResolution>75</scan:XResolution><scan:YResolution>75</scan:YResolution></scan:DiscreteResolution>
<scan:DiscreteResolution><scan:XResolution>150</scan:XResolution><scan:YResolution>150</scan:YResolution></scan:DiscreteResolution>
<scan:DiscreteResolution><scan:XResolution>300</scan:XResolution><scan:YResolution>300</scan:YResolution></scan:DiscreteResolution>
<scan:DiscreteResolution><scan:XResolution>600</scan:XResolution><scan:YResolution>600</scan:YResolution></scan:DiscreteResolution>
</scan:DiscreteResolutions></scan:SupportedResolutions>
</scan:SettingProfile></scan:SettingProfiles>
<scan:SupportedIntents><scan:Intent>Document</scan:Intent><scan:Intent>Photo</scan:Intent></scan:SupportedIntents>
</scan:PlatenInputCaps></scan:Platen>
</scan:ScannerCapabilities>`

// Scanner is a fake eSCL flatbed scanner. Every scan delivers one page, a
// picture of the size and colour mode that was asked for.
type Scanner struct {
	*httptest.Server

	mu       sync.Mutex
	next     int
	jobs     map[string][]byte // job id -> page not fetched yet
	Requests []string          // ScanSettings documents received
	// Busy makes the scanner refuse new jobs with 503.
	Busy bool
}

// NewScanner starts a fake scanner.
func NewScanner(t *testing.T) *Scanner {
	s := &Scanner{jobs: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// BaseURL returns the scanner's eSCL address.
func (s *Scanner) BaseURL() string { return s.URL + "/eSCL" }

var (
	reWidth  = regexp.MustCompile(`<pwg:Width>(\d+)</pwg:Width>`)
	reHeight = regexp.MustCompile(`<pwg:Height>(\d+)</pwg:Height>`)
	reRes    = regexp.MustCompile(`<scan:XResolution>(\d+)</scan:XResolution>`)
	reJob    = regexp.MustCompile(`^/eSCL/ScanJobs/([a-z0-9-]+)(/NextDocument)?$`)
)

func (s *Scanner) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/eSCL/ScannerCapabilities":
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, Capabilities)

	case r.Method == http.MethodGet && r.URL.Path == "/eSCL/ScannerStatus":
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, `<?xml version="1.0"?><scan:ScannerStatus xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm"><pwg:Version>2.63</pwg:Version><pwg:State>Idle</pwg:State></scan:ScannerStatus>`)

	case r.Method == http.MethodPost && r.URL.Path == "/eSCL/ScanJobs":
		if s.Busy {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.Requests = append(s.Requests, string(body))
		num := func(re *regexp.Regexp) int {
			if m := re.FindSubmatch(body); m != nil {
				n, _ := strconv.Atoi(string(m[1]))
				return n
			}
			return 0
		}
		dpi := num(reRes)
		// The region is in 1/300 inch.
		width, height := num(reWidth)*dpi/300, num(reHeight)*dpi/300
		s.next++
		id := fmt.Sprintf("job-%d", s.next)
		s.jobs[id] = page(width, height, s.next, bytes.Contains(body, []byte("Grayscale8")))
		// Like real scanners, answer with a host name the client can't resolve.
		w.Header().Set("Location", "http://scanner-with-mdns-name.local/eSCL/ScanJobs/"+id)
		w.WriteHeader(http.StatusCreated)

	case reJob.MatchString(r.URL.Path):
		m := reJob.FindStringSubmatch(r.URL.Path)
		data, ok := s.jobs[m[1]]
		switch {
		case r.Method == http.MethodDelete:
			delete(s.jobs, m[1])
		case m[2] == "" || !ok || data == nil:
			http.NotFound(w, r)
		default:
			s.jobs[m[1]] = nil // a flatbed job has one page
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
		}

	default:
		http.NotFound(w, r)
	}
}

// page draws a test page: paper white with a dark band whose position depends
// on the page number, so pages can be told apart.
func page(width, height, n int, gray bool) []byte {
	var img interface {
		image.Image
		Set(x, y int, c color.Color)
	}
	if gray {
		img = image.NewGray(image.Rect(0, 0, width, height))
	} else {
		img = image.NewRGBA(image.Rect(0, 0, width, height))
	}
	band := (n * height / 10) % height
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := color.RGBA{250, 248, 240, 255}
			if y >= band && y < band+height/20 {
				c = color.RGBA{30, 60, 160, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
