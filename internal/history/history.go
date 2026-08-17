// Package history persists per-manga reading progress on the server,
// so reading resumes from any device. Backed by the same file store as
// the library (data/history.json).
package history

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"tsukimi/internal/domain"
	"tsukimi/internal/store"
)

type Service struct {
	st    *store.Store
	mu    sync.RWMutex
	cache map[string]domain.ReadingProgress
}

func New(st *store.Store) (*Service, error) {
	s := &Service{st: st, cache: map[string]domain.ReadingProgress{}}
	var list []domain.ReadingProgress
	if err := st.Read("history", &list); err != nil {
		return nil, err
	}
	for _, p := range list {
		s.cache[key(p.SourceID, p.MangaID)] = p
	}
	return s, nil
}

func key(sourceID, mangaID string) string { return sourceID + ":" + mangaID }

// Get returns the saved progress for a manga, if any.
func (s *Service) Get(sourceID, mangaID string) (domain.ReadingProgress, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.cache[key(sourceID, mangaID)]
	return p, ok
}

// Set upserts progress for a manga.
func (s *Service) Set(p domain.ReadingProgress) error {
	if p.ChapterID == "" {
		return fmt.Errorf("chapter_id required")
	}
	if p.Page < 1 {
		p.Page = 1
	}
	p.UpdatedAt = time.Now()
	s.mu.Lock()
	s.cache[key(p.SourceID, p.MangaID)] = p
	snapshot := s.snapshot()
	s.mu.Unlock()
	return s.st.Write("history", snapshot)
}

// Delete removes progress for a manga (used when removing from library).
func (s *Service) Delete(sourceID, mangaID string) error {
	s.mu.Lock()
	delete(s.cache, key(sourceID, mangaID))
	snapshot := s.snapshot()
	s.mu.Unlock()
	return s.st.Write("history", snapshot)
}

// List returns all saved progress, most recent first.
func (s *Service) List() []domain.ReadingProgress {
	s.mu.RLock()
	out := make([]domain.ReadingProgress, 0, len(s.cache))
	for _, p := range s.cache {
		out = append(out, p)
	}
	s.mu.RUnlock()
	// 稳定的 map 输出：按时间倒序，便于前端展示
	sortByUpdatedDesc(out)
	return out
}

func (s *Service) snapshot() []domain.ReadingProgress {
	out := make([]domain.ReadingProgress, 0, len(s.cache))
	for _, p := range s.cache {
		out = append(out, p)
	}
	return out
}

func sortByUpdatedDesc(list []domain.ReadingProgress) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].UpdatedAt.After(list[j].UpdatedAt)
	})
}
