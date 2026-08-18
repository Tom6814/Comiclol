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
	// orderSeq 是全局入库顺序计数器，每次有新漫画入库时自增并赋值给其 Order。
	// 持久化在 store 的 "library_order" 键，重启后续号，保证顺序稳定。
	orderSeq int
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
	// 恢复顺序计数器；同时给历史数据（无 Order）补一个稳定的 Order。
	maxOrder := 0
	for _, m := range s.cache {
		if m.Order > maxOrder {
			maxOrder = m.Order
		}
	}
	// 任何缺 Order 的旧记录，按当前最大序号续号（保持相对先后）。
	needsOrder := false
	for _, m := range s.cache {
		if m.Order == 0 {
			needsOrder = true
			break
		}
	}
	if needsOrder {
		// 按 AddedAt/UpdatedAt 升序补号，尽量保留历史先后；全零则任意但稳定。
		type pair struct{ k string; m domain.Manga }
		pairs := make([]pair, 0, len(s.cache))
		for k, m := range s.cache {
			pairs = append(pairs, pair{k, m})
		}
		sort.Slice(pairs, func(i, j int) bool {
			ti := pairs[i].m.AddedAt
			if ti.IsZero() {
				ti = pairs[i].m.UpdatedAt
			}
			tj := pairs[j].m.AddedAt
			if tj.IsZero() {
				tj = pairs[j].m.UpdatedAt
			}
			return ti.Before(tj)
		})
		for _, p := range pairs {
			if p.m.Order == 0 {
				maxOrder++
				p.m.Order = maxOrder
				s.cache[p.k] = p.m
			}
		}
		_ = st.Write("library", s.snapshot())
	}
	s.orderSeq = maxOrder
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
		// 保留已下载状态：Upsert 用的是远端拉来的元数据（Downloaded 默认 false），
		// 不能覆盖本地已确立的下载完成标记。
		m.Downloaded = existing.Downloaded
		m.LocalPath = existing.LocalPath
		// 已入库记录保留原 Order（稳定排序依据），不被覆盖。
		m.Order = existing.Order
	} else {
		// 新入库：分配下一个顺序号（越大越新入库）。
		s.orderSeq++
		m.Order = s.orderSeq
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

// List returns all manga in library-entry order matching the remote favorites
// order: 最新收藏在前。Order 是入库顺序号（同步时按远端收藏顺序赋值），
// JM 收藏列表默认最新在前，所以第一条入库 Order 最小；要「最新在前」
// 就按 Order 降序返回。完全不依赖任何时间戳。
func (s *Service) List() []domain.Manga {
	s.mu.RLock()
	out := make([]domain.Manga, 0, len(s.cache))
	for _, m := range s.cache {
		out = append(out, m)
	}
	s.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		// 都有 Order：降序（新入库在前）。
		if out[i].Order != 0 && out[j].Order != 0 {
			return out[i].Order > out[j].Order
		}
		// 兜底：Order 缺失的旧记录，退化为 AddedAt 降序。
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

// IsDownloaded reports whether any chapter images exist on disk for a manga.
// 这是判断「是否已下载」的权威依据（文件在 $DATA_DIR 持久化），
// 不依赖 library.json 里的 Downloaded 布尔标记——后者可能因重新部署、
// 元数据未及时落盘等原因失真。
func (s *Service) IsDownloaded(sourceID, mangaID string) bool {
	dir := s.MangaDir(sourceID, mangaID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			if !f.IsDir() && isImage(f.Name()) {
				return true
			}
		}
	}
	return false
}

// ReconcileDownloaded 按磁盘实际文件校正每本漫画的 Downloaded 标记。
// 启动时调用一次，确保重新部署后（/data 持久化、library.json 可能失真）
// 书库列表的「已下载」徽标和详情页主按钮都与磁盘一致。
// 返回被修正的记录数。
func (s *Service) ReconcileDownloaded() int {
	s.mu.Lock()
	ids := make([][2]string, 0, len(s.cache))
	for k := range s.cache {
		ids = append(ids, splitKey(k))
	}
	s.mu.Unlock()

	changed := 0
	for _, id := range ids {
		sourceID, mangaID := id[0], id[1]
		onDisk := s.IsDownloaded(sourceID, mangaID)
		s.mu.Lock()
		m, ok := s.cache[key(sourceID, mangaID)]
		if !ok {
			s.mu.Unlock()
			continue
		}
		if m.Downloaded != onDisk {
			m.Downloaded = onDisk
			if onDisk {
				m.LocalPath = s.MangaDir(sourceID, mangaID)
			}
			s.cache[key(sourceID, mangaID)] = m
			changed++
		}
		s.mu.Unlock()
	}
	if changed > 0 {
		_ = s.st.Write("library", s.snapshot())
	}
	return changed
}

// splitKey 把 "sourceID:mangaID" 拆开。与 key() 配对。
func splitKey(k string) [2]string {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return [2]string{k[:i], k[i+1:]}
		}
	}
	return [2]string{k, ""}
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
