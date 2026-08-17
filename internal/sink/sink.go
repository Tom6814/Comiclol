// Package sink defines upload destinations (cloud drives, etc.) as plugins.
//
// A Sink takes a downloaded manga directory and ships it somewhere. The local
// filesystem is the implicit default sink; additional sinks (WebDAV, alist,
// etc.) register through the same registry.
package sink

import (
	"context"
	"fmt"
	"sync"
)

// UploadJob describes one upload task handed to a Sink.
type UploadJob struct {
	Source   string            // source plugin id that produced the manga
	MangaID  string            // manga id
	Title    string            // human label
	LocalDir string            // directory containing the downloaded chapter(s)
	Meta     map[string]string // free-form metadata for the sink
}

// Result reports the outcome of an upload.
type Result struct {
	OK     bool
	URL    string
	Detail string
}

// Sink is the upload destination abstraction.
type Sink interface {
	ID() string
	DisplayName() string
	Configure(cfg map[string]any) error
	Upload(ctx context.Context, job UploadJob) (Result, error)
	Test(ctx context.Context) error
}

// Registry maps sink id -> Sink.
type Registry struct {
	mu    sync.RWMutex
	sinks map[string]Sink
	order []string
}

func NewRegistry() *Registry { return &Registry{sinks: map[string]Sink{}} }

func (r *Registry) Register(s Sink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sinks[s.ID()]; !ok {
		r.order = append(r.order, s.ID())
	}
	r.sinks[s.ID()] = s
}

func (r *Registry) Get(id string) (Sink, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sinks[id]
	return s, ok
}

func (r *Registry) List() []Sink {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Sink, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.sinks[id])
	}
	return out
}

func (r *Registry) MustGet(id string) (Sink, error) {
	s, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("sink %q not found", id)
	}
	return s, nil
}
