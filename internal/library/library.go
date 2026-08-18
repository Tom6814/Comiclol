// Package library manages the local manga collection: metadata persistence,
// on-disk layout for downloaded chapters, covers, and reading listings.
package library

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"tsukimi/internal/domain"
	"tsukimi/internal/store"
)

// MangaEntry wraps a manga with computed library fields.
type MangaEntry struct {
	domain.Manga
	HasFiles bool `json:"has_files"`
}

type Service struct {
	dataDir string
	st      *store.Store
	mu      sync.RWMutex
	cache   map[string]domain.Manga // in-memory copy for fast reads
}

func New(dataDir string, st *store.Store) (*Service, error) {
	s := &Service{
		dataDir: dataDir,
		st:      st,
		cache:   map[string]domain.Manga{},
	}
	var list []domain.Manga
	if err := st.Read("library", &list); err != nil {
		return nil, err
	}
	for _, m := range list {
		s.cache[key(m.SourceID, m.ID)] = m
	}
	if err := os.MkdirAll(s.LibraryDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.CoversDir(), 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) LibraryDir() string { return filepath.Join(s.dataDir, "library") }
func (s *Service) CoversDir() string  { return filepath.Join(s.dataDir, "covers") }
func (s *Service) CacheDir() string   { return filepath.Join(s.dataDir, "cache") }

func key(sourceID, mangaID string) string { return sourceID + ":" + mangaID }

func safeName(s string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "?", "\"", "_",
		"<", "_", ">", "_", "|", "_", "\n", "_", "\r", "_",
	)
	out := r.Replace(s)
	if len(out) > 80 {
		out = out[:80]
	}
	return strings.TrimSpace(out)
}

// MangaDir returns the on-disk directory for a manga's chapters.
func (s *Service) MangaDir(sourceID, mangaID string) string {
	return filepath.Join(s.LibraryDir(), fmt.Sprintf("%s_%s", sourceID, mangaID))
}

// ChapterDir returns the directory holding one chapter's images.
func (s *Service) ChapterDir(sourceID, mangaID, chapterID string) string {
	return filepath.Join(s.MangaDir(sourceID, mangaID), chapterID)
}

// CoverPath returns the local cover file path for a manga.
func (s *Service) CoverPath(sourceID, mangaID string) string {
	return filepath.Join(s.CoversDir(), fmt.Sprintf("%s_%s.jpg", sourceID, mangaID))
}

// Upsert persists manga metadata (merge with existing).
func (s *Service) Upsert(m domain.Manga) error {
	s.mu.Lock()
	existing, ok := s.cache[key(m.SourceID, m.ID)]
	if ok {
		if m.Title == "" {
			m.Title = existing.Title
		}
		if m.Author == "" {
			m.Author = existing.Author
		}
		if len(m.Chapters) == 0 {
			m.Chapters = existing.Chapters
		}
		if m.AddedAt.IsZero() {
			m.AddedAt = existing.AddedAt
		}
	}
	if m.AddedAt.IsZero() {
		m.AddedAt = now()
	}
	m.UpdatedAt = now()
	s.cache[key(m.SourceID, m.ID)] = m
	snapshot := s.snapshot()
	s.mu.Unlock()
	return s.st.Write("library", snapshot)
}

// Patch applies partial updates to a manga record.
func (s *Service) Patch(sourceID, mangaID string, fn func(*domain.Manga)) error {
	s.mu.Lock()
	m, ok := s.cache[key(sourceID, mangaID)]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("manga not found")
	}
	fn(&m)
	m.UpdatedAt = now()
	s.cache[key(sourceID, mangaID)] = m
	snapshot := s.snapshot()
	s.mu.Unlock()
	return s.st.Write("library", snapshot)
}

// Get returns a manga by id.
func (s *Service) Get(sourceID, mangaID string) (domain.Manga, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.cache[key(sourceID, mangaID)]
	return m, ok
}

// List returns all manga, newest added first (recently collected → oldest).
// AddedAt reflects when a manga first entered the library (≈ collect time);
// fall back to UpdatedAt when AddedAt is missing for any reason.
func (s *Service) List() []domain.Manga {
	s.mu.RLock()
	out := make([]domain.Manga, 0, len(s.cache))
	for _, m := range s.cache {
		out = append(out, m)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].AddedAt
		if ti.IsZero() {
			ti = out[i].UpdatedAt
		}
		tj := out[j].AddedAt
		if tj.IsZero() {
			tj = out[j].UpdatedAt
		}
		return ti.After(tj)
	})
	return out
}

// Remove deletes metadata and optionally its files.
func (s *Service) Remove(sourceID, mangaID string, deleteFiles bool) error {
	s.mu.Lock()
	delete(s.cache, key(sourceID, mangaID))
	snapshot := s.snapshot()
	dir := s.MangaDir(sourceID, mangaID)
	cover := s.CoverPath(sourceID, mangaID)
	s.mu.Unlock()
	if err := s.st.Write("library", snapshot); err != nil {
		return err
	}
	if deleteFiles {
		_ = os.RemoveAll(dir)
		_ = os.Remove(cover)
	}
	return nil
}

// ListPages returns sorted image file names for a chapter.
func (s *Service) ListPages(sourceID, mangaID, chapterID string) ([]string, error) {
	dir := s.ChapterDir(sourceID, mangaID, chapterID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isImage(e.Name()) {
			files = append(files, e.Name())
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return naturalLess(files[i], files[j])
	})
	return files, nil
}

// PageAbsPath returns the absolute path of a page file.
func (s *Service) PageAbsPath(sourceID, mangaID, chapterID, file string) string {
	return filepath.Join(s.ChapterDir(sourceID, mangaID, chapterID), file)
}

// HasChapterFiles reports whether a chapter has any downloaded images.
func (s *Service) HasChapterFiles(sourceID, mangaID, chapterID string) bool {
	files, err := s.ListPages(sourceID, mangaID, chapterID)
	return err == nil && len(files) > 0
}

func (s *Service) snapshot() []domain.Manga {
	out := make([]domain.Manga, 0, len(s.cache))
	for _, m := range s.cache {
		out = append(out, m)
	}
	return out
}

func isImage(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// naturalLess compares strings numerically where possible ("2" < "10").
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			// skip leading zeros
			startI, startJ := i, j
			for i < len(a) && a[i] == '0' {
				i++
			}
			for j < len(b) && b[j] == '0' {
				j++
			}
			numI, numJ := i, j
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			lenI, lenJ := i-numI, j-numJ
			if lenI != lenJ {
				return lenI < lenJ
			}
			_ = startI
			_ = startJ
			if a[numI:i] != b[numJ:j] {
				return a[numI:i] < b[numJ:j]
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
