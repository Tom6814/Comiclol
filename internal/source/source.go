// Package source defines the contract for manga providers and a registry
// the host uses to look them up by id. Adding a new provider means writing one
// plugin that calls Register() — nothing else in the app changes.
package source

import (
	"context"
	"fmt"
	"sync"

	"tsukimi/internal/domain"
)

// Source is the abstraction every manga provider implements.
//
// The methods deliberately mirror JMComic's surface (album/photo/image) so the
// reference behaviour maps cleanly, while staying generic enough for future
// providers.
type Source interface {
	ID() string
	DisplayName() string
	Login(ctx context.Context, creds domain.Credentials) (domain.Session, error)

	GetManga(ctx context.Context, sess domain.Session, mangaID string) (*domain.Manga, error)
	GetChapter(ctx context.Context, sess domain.Session, chapterID string) (*domain.Chapter, []domain.Page, error)

	Favorites(ctx context.Context, sess domain.Session, folderID string, page int) (*domain.FavoritePage, error)
	FavoriteFolders(ctx context.Context, sess domain.Session) ([]domain.Folder, error)

	// FetchImage downloads one raw (possibly scrambled) image.
	FetchImage(ctx context.Context, sess domain.Session, page domain.Page) (domain.ImageData, error)

	// Capabilities advertises optional features (search etc.).
	Capabilities() Capabilities
}

// Capabilities lets a source opt into optional behaviours.
type Capabilities struct {
	HasFavorites     bool
	HasSearch        bool
	SupportsLogin    bool
	MultiChapter     bool
	NeedsImageDecode bool
}

// Searcher is an optional interface a Source can implement.
type Searcher interface {
	Search(ctx context.Context, sess domain.Session, query string, page int) (*SearchResult, error)
}

// SearchResult is a page of search hits.
type SearchResult struct {
	Items []domain.Favorite `json:"items"`
	Page  int               `json:"page"`
	Pages int               `json:"pages"`
	Total int               `json:"total"`
}

// Registry maps source id -> Source.
type Registry struct {
	mu      sync.RWMutex
	sources map[string]Source
	order   []string
}

func NewRegistry() *Registry { return &Registry{sources: map[string]Source{}} }

func (r *Registry) Register(s Source) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[s.ID()]; ok {
		return fmt.Errorf("source %q already registered", s.ID())
	}
	r.sources[s.ID()] = s
	r.order = append(r.order, s.ID())
	return nil
}

func (r *Registry) Get(id string) (Source, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sources[id]
	return s, ok
}

func (r *Registry) List() []Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Source, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.sources[id])
	}
	return out
}
