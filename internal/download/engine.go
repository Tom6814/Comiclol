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
	ctx    context.Context
	cancel context.CancelFunc
}

type Options struct {
	ImageQuality int // JPEG 质量
}

type Engine struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	order []string // 任务提交顺序（用于列表稳定）

	queue   chan *Task
	srcReg  *source.Registry
	lib     *library.Service
	bus     *plugin.EventBus
	logger  *plugin.Logger
	opts    Options
	sess    *session.Manager

	wg sync.WaitGroup
}

func NewEngine(srcReg *source.Registry, lib *library.Service, bus *plugin.EventBus, logger *plugin.Logger, sess *session.Manager, opts Options) *Engine {
	return &Engine{
		tasks:  map[string]*Task{},
		queue:  make(chan *Task, 64),
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
	close(e.queue)
	e.wg.Wait()
}

func (e *Engine) loop() {
	defer e.wg.Done()
	for t := range e.queue {
		e.run(t)
	}
}

// Submit 创建并排队一个任务，返回任务 id。
func (e *Engine) Submit(sourceID, mangaID, title, origin string, chapterIDs []string) (string, error) {
	if _, ok := e.srcReg.Get(sourceID); !ok {
		return "", fmt.Errorf("未知来源 %q", sourceID)
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
	select {
	case e.queue <- t:
	default:
		go func() { e.queue <- t }()
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

	// 章节级并发
	var chapterWg sync.WaitGroup
	sem := make(chan struct{}, 4) // 同时最多 4 章
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
	imgSem := make(chan struct{}, 8) // 每章 8 张图并发
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

			e.bus.Emit(t.ctx, hook.DownloadImageBefore, hook.KeyMangaID, t.MangaID, hook.KeyChapterID, c.ID, hook.KeyPage, p.Index)

			data, err := src.FetchImage(t.ctx, sess, p)
			if err != nil {
				e.logger.Errorf("download", "取图 %s/%d 失败: %v", c.ID, p.Index, err)
				failed.Store(true)
				return
			}
			// 解码（去混淆）。aid 用章节 ID（photo_id），filename 用页面文件名。
			aid, _ := strconv.ParseInt(c.ID, 10, 64)
			out, err := img.Decode(data.Reader, data.Suffix, p.ScrambleID, aid, p.FileName, e.opts.ImageQuality)
			if err != nil {
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
				// 如果解码改变了后缀，更新最终文件名
				base := stripExt(filepath.Base(dst))
				final = filepath.Join(filepath.Dir(dst), base+out.Suffix)
			}
			if err := os.Rename(tmp, final); err != nil {
				e.logger.Errorf("download", "rename %s -> %s: %v", tmp, final, err)
				failed.Store(true)
				return
			}
			e.mu.Lock()
			t.Done++
			e.mu.Unlock()
			e.emitProgress(t)
			e.bus.Emit(t.ctx, hook.DownloadImageAfter, hook.KeyMangaID, t.MangaID, hook.KeyChapterID, c.ID, hook.KeyPage, p.Index, "path", final)
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
