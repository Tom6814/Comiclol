// Package download 是下载引擎。
//
// 设计要点：
//   - Engine 持有一个任务表和一个有界工作 channel，串行消费任务，
//     任务内部章节并发、章节内部图片并发，形成两级并发。
//   - 断点续传基于「目标文件已存在且大小>0 则跳过」，无需额外状态文件，
//     与文件系统天然对齐，重启即恢复。
//   - 所有进度、完成、失败事件通过 plugin.EventBus 广播，插件可观测/干预。
//   - 取消通过 context 传播；任务记录状态机：queued→running→done/failed/canceled。
package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"tsukimi/internal/domain"
	"tsukimi/internal/hook"
	"tsukimi/internal/img"
	"tsukimi/internal/library"
	"tsukimi/internal/plugin"
	"tsukimi/internal/session"
	"tsukimi/internal/source"
)

// 状态机
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusCanceled = "canceled"
)

// Task 表示一次漫画下载请求。
type Task struct {
	ID         string    `json:"id"`
	SourceID   string    `json:"source_id"`
	MangaID    string    `json:"manga_id"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	Progress   float64   `json:"progress"`   // 0..1
	Total      int       `json:"total"`      // 总图片数
	Done       int       `json:"done"`       // 已完成图片数
	ChapterIDs []string  `json:"chapter_ids"` // 指定章节（空=全部）
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error,omitempty"`
	Origin     string    `json:"origin,omitempty"` // manual / sync / ...

	// 运行时字段（不持久化）
	ctx     context.Context
	cancel  context.CancelFunc
	yielded bool // 已让位给单话任务（防止无限插队）
}

type Options struct {
	ImageQuality int // JPEG 质量
	ChapterJobs  int // 单个任务内并发下载的章节数（默认 4）
	ImageWorkers int // 单章内并发下载的图片数（默认 8）
	// ImageRetries：单张图片下载失败后的重试次数（默认 3）。
	// 单张图瞬时网络失败不再判整章失败，避免「部分章节下载失败」。
	ImageRetries int
}

type Engine struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	order []string // 任务提交顺序（用于列表稳定）

	// 两级优先级队列：highQ 放新任务（单话优先），lowQ 放多话让位任务。
	// worker 永远优先消费 highQ，highQ 清空后才轮到 lowQ，实现「单话优先」。
	highQ chan *Task
	lowQ  chan *Task
	srcReg  *source.Registry
	lib     *library.Service
	bus     *plugin.EventBus
	logger  *plugin.Logger
	opts    Options
	sess    *session.Manager

	wg sync.WaitGroup
}

func NewEngine(srcReg *source.Registry, lib *library.Service, bus *plugin.EventBus, logger *plugin.Logger, sess *session.Manager, opts Options) *Engine {
	if opts.ChapterJobs <= 0 {
		opts.ChapterJobs = 4
	}
	if opts.ImageWorkers <= 0 {
		opts.ImageWorkers = 8
	}
	return &Engine{
		tasks:  map[string]*Task{},
		highQ:  make(chan *Task, 64),
		lowQ:   make(chan *Task, 256),
		srcReg: srcReg,
		lib:    lib,
		bus:    bus,
		logger: logger,
		opts:   opts,
		sess:   sess,
	}
}

func (e *Engine) Start(workers int) {
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		e.wg.Add(1)
		go e.loop()
	}
}

func (e *Engine) Stop() {
	close(e.highQ)
	close(e.lowQ)
	e.wg.Wait()
}

func (e *Engine) loop() {
	defer e.wg.Done()
	for {
		// 优先消费高优队列（新任务/单话），空后才取低优队列（多话让位）。
		var t *Task
		select {
		case t = <-e.highQ:
		default:
			select {
			case t = <-e.highQ:
			case t = <-e.lowQ:
			}
		}
		if t == nil {
			return // 两个队列都已关闭
		}
		e.run(t)
	}
}

// Submit 创建并排队一个任务，返回任务 id。
func (e *Engine) Submit(sourceID, mangaID, title, origin string, chapterIDs []string) (string, error) {
	if _, ok := e.srcReg.Get(sourceID); !ok {
		return "", fmt.Errorf("未知来源 %q", sourceID)
	}
	// 任务级去重：同一 (sourceID, mangaID) 已有排队中/运行中的任务时，复用它，
	// 避免同一本漫画被重复提交（如同一收藏在同步列表里重复出现、或收藏了
	// 整本+某一话的同一 album）。已完成或已失败的不复用。
	e.mu.RLock()
	var existing string
	for _, tid := range e.order {
		t := e.tasks[tid]
		if t.SourceID == sourceID && t.MangaID == mangaID {
			if t.Status == StatusQueued || t.Status == StatusRunning {
				existing = tid
				break
			}
		}
	}
	e.mu.RUnlock()
	if existing != "" {
		return existing, nil
	}
	id := newID()
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID:         id,
		SourceID:   sourceID,
		MangaID:    mangaID,
		Title:      title,
		Status:     StatusQueued,
		ChapterIDs: chapterIDs,
		CreatedAt:  time.Now(),
		Origin:     origin,
		ctx:        ctx,
		cancel:     cancel,
	}
	e.mu.Lock()
	e.tasks[id] = t
	e.order = append(e.order, id)
	e.mu.Unlock()
	e.bus.Emit(ctx, hook.DownloadQueued, hook.KeyTaskID, id, hook.KeySourceID, sourceID, hook.KeyMangaID, mangaID)
	// 新任务一律进高优队列（可能是单话，先让 worker 取到再判定）。
	select {
	case e.highQ <- t:
	default:
		go func() { e.highQ <- t }()
	}
	return id, nil
}

// Cancel 取消一个运行中或排队的任务。
func (e *Engine) Cancel(id string) bool {
	e.mu.RLock()
	t, ok := e.tasks[id]
	e.mu.RUnlock()
	if !ok {
		return false
	}
	if t.cancel != nil {
		t.cancel()
	}
	return true
}

// Remove 删除一个任务记录。
func (e *Engine) Remove(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tasks[id]
	if !ok {
		return false
	}
	if t.cancel != nil && t.Status == StatusRunning {
		t.cancel()
	}
	delete(e.tasks, id)
	for i, x := range e.order {
		if x == id {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	return true
}

// Get 返回任务副本。
func (e *Engine) Get(id string) (Task, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	t, ok := e.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *t, true
}

// List 返回所有任务（最新优先）。
func (e *Engine) List() []Task {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Task, 0, len(e.order))
	// 倒序：最新提交在前
	for i := len(e.order) - 1; i >= 0; i-- {
		if t, ok := e.tasks[e.order[i]]; ok {
			out = append(out, *t)
		}
	}
	return out
}

func (e *Engine) setStatus(t *Task, status string) {
	e.mu.Lock()
	t.Status = status
	e.mu.Unlock()
}

func (e *Engine) run(t *Task) {
	setStatus := func(s string) { e.setStatus(t, s) }
	t.StartedAt = time.Now()
	setStatus(StatusRunning)
	e.bus.Emit(t.ctx, hook.DownloadStart, hook.KeyTaskID, t.ID, hook.KeyMangaID, t.MangaID)

	src, ok := e.srcReg.Get(t.SourceID)
	if !ok {
		e.fail(t, fmt.Errorf("来源未注册: %s", t.SourceID))
		return
	}

	sess, _ := e.sess.Get(t.SourceID) // 未登录则空会话；能否匿名下载由来源决定
	manga, err := src.GetManga(t.ctx, sess, t.MangaID)
	if err != nil {
		e.fail(t, err)
		return
	}
	if t.Title == "" {
		t.Title = manga.Title
	}

	// 决定要下载的章节
	chapters := manga.Chapters
	if len(t.ChapterIDs) > 0 {
		want := map[string]bool{}
		for _, id := range t.ChapterIDs {
			want[id] = true
		}
		var picked []domain.Chapter
		for _, c := range chapters {
			if want[c.ID] {
				picked = append(picked, c)
			}
		}
		chapters = picked
	}

	// 单话优先：多话任务让位给单话（单话先下完，多话排后）。
	// 让位只发生一次（yielded），下次被取出时直接执行。
	if len(chapters) > 1 && !t.yielded {
		e.mu.Lock()
		t.yielded = true
		t.Status = StatusQueued
		e.mu.Unlock()
		e.logger.Infof("download", "任务 %s 多话(%d章) 让位给单话任务，转入低优队列", t.MangaID, len(chapters))
		select {
		case e.lowQ <- t:
		default:
			go func() { e.lowQ <- t }()
		}
		return
	}

	// 预计算总图片数（用于进度）；同时把已完成的本地章节计入。
	total := 0
	done := 0
	for _, c := range chapters {
		total += c.PageCount
		if e.lib.HasChapterFiles(t.SourceID, t.MangaID, c.ID) {
			files, _ := e.lib.ListPages(t.SourceID, t.MangaID, c.ID)
			done += len(files)
		}
	}
	e.mu.Lock()
	t.Total = total
	t.Done = done
	e.mu.Unlock()
	e.emitProgress(t)

	// 落库元数据（即使下载未完成也能在前端显示）
	if err := e.lib.Upsert(*manga); err != nil {
		e.logger.Warnf("download", "保存元数据失败: %v", err)
	}

	// 章节级并发（单任务内多线程）
	var chapterWg sync.WaitGroup
	sem := make(chan struct{}, e.opts.ChapterJobs) // 任务内同时下载的章节数
	var failed atomic.Bool

	for _, c := range chapters {
		if e.lib.HasChapterFiles(t.SourceID, t.MangaID, c.ID) {
			// 已存在视为完成，但仍校验计数
			continue
		}
		chapterWg.Add(1)
		go func(c domain.Chapter) {
			defer chapterWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := e.downloadChapter(t, src, manga, c); err != nil {
				e.logger.Errorf("download", "章节 %s 失败: %v", c.ID, err)
				failed.Store(true)
			}
		}(c)
	}
	chapterWg.Wait()

	if t.ctx.Err() != nil {
		setStatus(StatusCanceled)
		e.bus.Emit(context.Background(), hook.DownloadFailed, hook.KeyTaskID, t.ID, hook.KeyErr, "canceled")
		return
	}

	t.FinishedAt = time.Now()
	e.mu.Lock()
	t.Progress = 1.0
	e.mu.Unlock()

	if failed.Load() {
		e.fail(t, fmt.Errorf("部分章节下载失败"))
		return
	}
	setStatus(StatusDone)
	e.bus.Emit(context.Background(), hook.DownloadComplete, hook.KeyTaskID, t.ID, hook.KeyMangaID, t.MangaID, "path", e.lib.MangaDir(t.SourceID, t.MangaID))

	// 标记本地已下载
	_ = e.lib.Patch(t.SourceID, t.MangaID, func(m *domain.Manga) {
		m.Downloaded = true
		m.LocalPath = e.lib.MangaDir(t.SourceID, t.MangaID)
	})
}

func (e *Engine) downloadChapter(t *Task, src source.Source, manga *domain.Manga, c domain.Chapter) error {
	sess, _ := e.sess.Get(t.SourceID)
	_, pages, err := src.GetChapter(t.ctx, sess, c.ID)
	if err != nil {
		return err
	}
	dir := e.lib.ChapterDir(t.SourceID, t.MangaID, c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var wg sync.WaitGroup
	imgSem := make(chan struct{}, e.opts.ImageWorkers) // 单章内并发图片数
	var failed atomic.Bool
	for _, p := range pages {
		p := p
		// 断点续传：文件已存在则跳过
		dst := filepath.Join(dir, finalName(p))
		if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
			e.mu.Lock()
			t.Done++
			e.mu.Unlock()
			e.emitProgress(t)
			continue
		}
		// 临时文件先写，成功后改名（防止半成品被误判已完成）
		tmp := dst + ".part"

		wg.Add(1)
		go func() {
			defer wg.Done()
			imgSem <- struct{}{}
			defer func() { <-imgSem }()

			retries := e.opts.ImageRetries
			if retries <= 0 {
				retries = 3
			}
			var lastErr error
			for attempt := 0; attempt <= retries; attempt++ {
				if attempt > 0 {
					// 指数退避：1s, 2s, 4s...（上限 8s），避免对 CF/CDN 雪上加霜
					backoff := time.Duration(1<<uint(attempt-1)) * time.Second
					if backoff > 8*time.Second {
						backoff = 8 * time.Second
					}
					select {
					case <-time.After(backoff):
					case <-t.ctx.Done():
						return
					}
					e.logger.Infof("download", "重试 %s/%d 第 %d/%d 次", c.ID, p.Index, attempt, retries)
				}
				e.bus.Emit(t.ctx, hook.DownloadImageBefore, hook.KeyMangaID, t.MangaID, hook.KeyChapterID, c.ID, hook.KeyPage, p.Index)

				data, err := src.FetchImage(t.ctx, sess, p)
				if err != nil {
					lastErr = err
					continue // 网络错误：值得重试
				}
				aid, _ := strconv.ParseInt(c.ID, 10, 64)
				out, err := img.Decode(data.Reader, data.Suffix, p.ScrambleID, aid, p.FileName, e.opts.ImageQuality)
				if err != nil {
					// 解码失败通常是数据本身问题，重试也救不回；直接判失败
					e.logger.Errorf("download", "解码 %s/%d 失败: %v", c.ID, p.Index, err)
					failed.Store(true)
					return
				}
				if err := os.WriteFile(tmp, out.Data, 0o644); err != nil {
					e.logger.Errorf("download", "写文件 %s 失败: %v", tmp, err)
					failed.Store(true)
					return
				}
				final := dst
				if out.Suffix != "" {
					base := stripExt(filepath.Base(dst))
					final = filepath.Join(filepath.Dir(dst), base+out.Suffix)
				}
				if err := os.Rename(tmp, final); err != nil {
					e.logger.Errorf("download", "rename %s -> %s: %v", tmp, final, err)
					failed.Store(true)
					return
				}
				lastErr = nil
				e.bus.Emit(t.ctx, hook.DownloadImageAfter, hook.KeyMangaID, t.MangaID, hook.KeyChapterID, c.ID, hook.KeyPage, p.Index, "path", final)
				break // 成功
			}
			if lastErr != nil {
				e.logger.Errorf("download", "取图 %s/%d 失败（重试 %d 次后）: %v", c.ID, p.Index, retries, lastErr)
				failed.Store(true)
				return
			}
			e.mu.Lock()
			t.Done++
			e.mu.Unlock()
			e.emitProgress(t)
		}()
	}
	wg.Wait()

	e.bus.Emit(t.ctx, hook.DownloadChapterDone, hook.KeyMangaID, t.MangaID, hook.KeyChapterID, c.ID, "page_count", c.PageCount)
	if failed.Load() {
		return fmt.Errorf("章节 %s 部分图片失败", c.ID)
	}
	return nil
}

func (e *Engine) fail(t *Task, err error) {
	e.mu.Lock()
	t.Status = StatusFailed
	t.Error = err.Error()
	t.FinishedAt = time.Now()
	e.mu.Unlock()
	e.logger.Errorf("download", "任务 %s 失败: %v", t.ID, err)
	e.bus.Emit(context.Background(), hook.DownloadFailed, hook.KeyTaskID, t.ID, hook.KeyErr, err.Error())
}

func (e *Engine) emitProgress(t *Task) {
	e.mu.RLock()
	total := t.Total
	done := t.Done
	if total > 0 {
		t.Progress = float64(done) / float64(total)
	}
	e.mu.RUnlock()
	e.bus.Emit(context.Background(), hook.DownloadProgress, hook.KeyTaskID, t.ID, hook.KeyDone, done, hook.KeyTotal, total, "progress", t.Progress)
}

// 辅助函数
func finalName(p domain.Page) string {
	return fmt.Sprintf("%04d%s", p.Index, pSuffix(p))
}
func pSuffix(p domain.Page) string {
	// 下载时还不知道解码后的格式，先用 .jpg 占位；解码后会改名。
	return ".jpg"
}
func stripExt(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

var idCounter atomic.Uint64

func newID() string {
	n := idCounter.Add(1)
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), n)
}
