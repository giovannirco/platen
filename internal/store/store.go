// Package store keeps Platen's state on disk: scans (pages and finished
// documents) and the history of print jobs. Everything is plain files under one
// data directory, so a backup is a copy of that directory.
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a scan, page or job doesn't exist.
var ErrNotFound = errors.New("not found")

// Store is the on-disk state.
type Store struct {
	dir string

	mu   sync.Mutex
	jobs []PrintJob
}

// Open prepares the data directory and loads the job history.
func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "scans")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("data dir: %w", err)
		}
	}
	s := &Store{dir: dir}
	raw, err := os.ReadFile(s.jobsFile())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.jobs); err != nil {
			return nil, fmt.Errorf("read %s: %w", s.jobsFile(), err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return s, nil
}

// NewID returns a short random identifier with a prefix, e.g. "scn_k3f9x2ab7q".
func NewID(prefix string) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system random source is gone; nothing sensible can continue
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return prefix + "_" + string(b[:])
}

// ---- print job history -------------------------------------------------------

// PrintJob is one entry of the print history.
type PrintJob struct {
	ID         string    `json:"id"`
	Printer    string    `json:"printer"`
	PrinterJob int       `json:"printer_job_id"`
	Title      string    `json:"title"`
	Source     string    `json:"source"` // upload, url, file, paperless, text, scan
	Format     string    `json:"format"` // what was sent to the printer
	Pages      int       `json:"pages"`
	Copies     int       `json:"copies"`
	Sheets     int       `json:"sheets"`
	Duplex     string    `json:"duplex,omitempty"`
	Color      string    `json:"color,omitempty"`
	Quality    string    `json:"quality,omitempty"`
	Media      string    `json:"media,omitempty"`
	State      string    `json:"state"`
	Message    string    `json:"message,omitempty"`
	Via        string    `json:"via,omitempty"` // web, api, mcp
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const maxJobs = 500

func (s *Store) jobsFile() string { return filepath.Join(s.dir, "print-jobs.json") }

func (s *Store) saveJobsLocked() error {
	raw, err := json.MarshalIndent(s.jobs, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.jobsFile(), raw)
}

// AddJob records a new print job.
func (s *Store) AddJob(j PrintJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > maxJobs {
		s.jobs = s.jobs[len(s.jobs)-maxJobs:]
	}
	return s.saveJobsLocked()
}

// UpdateJob changes a recorded job through fn.
func (s *Store) UpdateJob(id string, fn func(*PrintJob)) (PrintJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			fn(&s.jobs[i])
			s.jobs[i].UpdatedAt = time.Now()
			return s.jobs[i], s.saveJobsLocked()
		}
	}
	return PrintJob{}, ErrNotFound
}

// Job returns one recorded job.
func (s *Store) Job(id string) (PrintJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return PrintJob{}, ErrNotFound
}

// Jobs returns the history, newest first.
func (s *Store) Jobs(limit int) []PrintJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PrintJob, 0, len(s.jobs))
	for i := len(s.jobs) - 1; i >= 0; i-- {
		out = append(out, s.jobs[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// ---- scans -------------------------------------------------------------------

// Scan is a scanning session: one or more pages and, once finished, a document.
type Scan struct {
	ID        string     `json:"id"`
	Scanner   string     `json:"scanner"`
	Title     string     `json:"title,omitempty"`
	Source    string     `json:"source"` // flatbed or feeder
	Color     string     `json:"color"`  // color or gray
	DPI       int        `json:"dpi"`
	Paper     string     `json:"paper"`
	Pages     []ScanPage `json:"pages"`
	Document  *ScanDoc   `json:"document,omitempty"`
	Paperless *Filed     `json:"paperless,omitempty"`
	Via       string     `json:"via,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ScanPage is one scanned page.
type ScanPage struct {
	// ID is stable for the life of the page; the position in Pages is the page number.
	ID     string `json:"id"`
	File   string `json:"-"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
}

// ScanDoc is the finished document of a scan.
type ScanDoc struct {
	File   string `json:"-"`
	Format string `json:"format"` // pdf or jpeg
	Bytes  int64  `json:"bytes"`
	Pages  int    `json:"pages"`
}

// Filed records that a scan was sent to Paperless.
type Filed struct {
	TaskID     string    `json:"task_id"`
	DocumentID int       `json:"document_id,omitempty"`
	URL        string    `json:"url,omitempty"`
	Status     string    `json:"status"`
	Message    string    `json:"message,omitempty"`
	At         time.Time `json:"at"`
}

var scanIDPattern = regexp.MustCompile(`^scn_[a-z0-9]{10}$`)

func (s *Store) scanDir(id string) (string, error) {
	if !scanIDPattern.MatchString(id) {
		return "", ErrNotFound
	}
	return filepath.Join(s.dir, "scans", id), nil
}

// CreateScan starts a new scan session on disk.
func (s *Store) CreateScan(sc Scan) (*Scan, error) {
	sc.ID = NewID("scn")
	sc.CreatedAt, sc.UpdatedAt = time.Now(), time.Now()
	dir, _ := s.scanDir(sc.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &sc, s.SaveScan(&sc)
}

// SaveScan writes a scan's metadata.
func (s *Store) SaveScan(sc *Scan) error {
	dir, err := s.scanDir(sc.ID)
	if err != nil {
		return err
	}
	sc.UpdatedAt = time.Now()
	raw, err := json.MarshalIndent(sc, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "scan.json"), raw)
}

// Scan loads a scan.
func (s *Store) Scan(id string) (*Scan, error) {
	dir, err := s.scanDir(id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "scan.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var sc Scan
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, fmt.Errorf("scan %s: %w", id, err)
	}
	for i := range sc.Pages {
		sc.Pages[i].File = filepath.Join(dir, sc.Pages[i].ID+".jpg")
	}
	if sc.Document != nil {
		ext := "pdf"
		if sc.Document.Format == "jpeg" {
			ext = "jpg"
		}
		sc.Document.File = filepath.Join(dir, "document."+ext)
	}
	return &sc, nil
}

// Scans lists scans, newest first.
func (s *Store) Scans(limit int) ([]*Scan, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "scans"))
	if err != nil {
		return nil, err
	}
	var out []*Scan
	for _, e := range entries {
		if !e.IsDir() || !scanIDPattern.MatchString(e.Name()) {
			continue
		}
		if sc, err := s.Scan(e.Name()); err == nil {
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AddPage stores a scanned page and appends it to the scan.
func (s *Store) AddPage(sc *Scan, jpeg []byte, width, height int) (*ScanPage, error) {
	dir, err := s.scanDir(sc.ID)
	if err != nil {
		return nil, err
	}
	page := ScanPage{ID: NewID("pg"), Width: width, Height: height, Bytes: int64(len(jpeg))}
	page.File = filepath.Join(dir, page.ID+".jpg")
	if err := writeFileAtomic(page.File, jpeg); err != nil {
		return nil, err
	}
	sc.Pages = append(sc.Pages, page)
	sc.Document = nil // the document no longer matches the pages
	return &sc.Pages[len(sc.Pages)-1], s.SaveScan(sc)
}

// RemovePage deletes a page from a scan.
func (s *Store) RemovePage(sc *Scan, pageID string) error {
	for i, p := range sc.Pages {
		if p.ID == pageID {
			_ = os.Remove(p.File)
			_ = os.Remove(ThumbPath(p.File))
			sc.Pages = append(sc.Pages[:i], sc.Pages[i+1:]...)
			sc.Document = nil
			return s.SaveScan(sc)
		}
	}
	return ErrNotFound
}

// MovePage moves a page to a new position (0-based).
func (s *Store) MovePage(sc *Scan, pageID string, to int) error {
	from := -1
	for i, p := range sc.Pages {
		if p.ID == pageID {
			from = i
		}
	}
	if from < 0 {
		return ErrNotFound
	}
	to = max(0, min(to, len(sc.Pages)-1))
	page := sc.Pages[from]
	sc.Pages = append(sc.Pages[:from], sc.Pages[from+1:]...)
	sc.Pages = append(sc.Pages[:to], append([]ScanPage{page}, sc.Pages[to:]...)...)
	sc.Document = nil
	return s.SaveScan(sc)
}

// DocumentPath returns where the finished document of a scan is written.
func (s *Store) DocumentPath(sc *Scan, format string) (string, error) {
	dir, err := s.scanDir(sc.ID)
	if err != nil {
		return "", err
	}
	ext := "pdf"
	if format == "jpeg" {
		ext = "jpg"
	}
	return filepath.Join(dir, "document."+ext), nil
}

// DeleteScan removes a scan and its files.
func (s *Store) DeleteScan(id string) error {
	dir, err := s.scanDir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		return ErrNotFound
	}
	return os.RemoveAll(dir)
}

// Prune deletes scans last changed before the cutoff. It returns how many were removed.
func (s *Store) Prune(cutoff time.Time) int {
	scans, err := s.Scans(0)
	if err != nil {
		return 0
	}
	n := 0
	for _, sc := range scans {
		if sc.UpdatedAt.Before(cutoff) {
			if s.DeleteScan(sc.ID) == nil {
				n++
			}
		}
	}
	return n
}

// ThumbPath returns the path of the cached thumbnail of a page file.
func ThumbPath(pageFile string) string {
	return strings.TrimSuffix(pageFile, ".jpg") + ".thumb.jpg"
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o640); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
