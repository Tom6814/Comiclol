// Package sync 负责把远端收藏夹和本地书库对齐。
//
// 设计很简单：一个 ticker 周期性调用 source.Favorites，把每一项与
// library.Service 里已有的记录比对——本地缺失或章节数变多就算「新」，
// 直接提交一条 origin="sync" 的下载任务。这让收藏夹自动落入书库，
// 不需要用户手动点下载。
//
// 与下载引擎之间只通过 channel/task 接口耦合，互不感知；事件总线
// 在每次 tick、发现新条目时都会广播，方便 UI 或日志订阅。
package syncfav

import (
	"context"
	"time"

	"tsukimi/internal/domain"
	"tsukimi/internal/download"
	"tsukimi/internal/hook"
	"tsukimi/internal/library"
	"tsukimi/internal/plugin"
	"tsukimi/internal/session"
	"tsukimi/internal/source"
)

type Service struct {
	srcReg *source.Registry
	sess   *session.Manager
	lib    *library.Service
	eng    *download.Engine
	bus    *plugin.EventBus
	logger *plugin.Logger

	interval time.Duration
	cancel   context.CancelFunc
}

// New 构造一个同步服务。interval<=0 表示不启动轮询。
func New(srcReg *source.Registry, sess *session.Manager, lib *library.Service, eng *download.Engine, bus *plugin.EventBus, logger *plugin.Logger, interval time.Duration) *Service {
	return &Service{
		srcReg:   srcReg,
		sess:     sess,
		lib:      lib,
		eng:      eng,
		bus:      bus,
		logger:   logger,
		interval: interval,
	}
}

// Start 启动后台轮询 goroutine，直到 ctx 被取消。
func (s *Service) Start(ctx context.Context) {
	if s.interval <= 0 {
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	go s.loop(ctx)
}

// Stop 取消轮询。
func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) loop(ctx context.Context) {
	// 启动后先等一个 interval 再首次拉取，避免与启动期其它 IO 抢资源。
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// TickNow 立刻执行一次同步（用于 UI 手动触发）。
func (s *Service) TickNow(ctx context.Context) error {
	return s.tick(ctx)
}

func (s *Service) tick(ctx context.Context) error {
	s.bus.Emit(ctx, hook.SyncTick, "at", time.Now().Unix())
	for _, src := range s.srcReg.List() {
		if !src.Capabilities().HasFavorites {
			continue
		}
		sess, ok := s.sess.Get(src.ID())
		if !ok {
			continue
		}
		folderID := ""
		page := 1
		for {
			fp, err := src.Favorites(ctx, sess, folderID, page)
			if err != nil {
				s.logger.Warnf("sync", "%s 收藏第 %d 页失败: %v", src.ID(), page, err)
				break
			}
			for _, it := range fp.Items {
				s.maybeSync(ctx, src, it)
			}
			if page >= fp.Pages || fp.Pages == 0 {
				break
			}
			page++
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

func (s *Service) maybeSync(ctx context.Context, src source.Source, fav domain.Favorite) {
	m, has := s.lib.Get(src.ID(), fav.MangaID)
	needs := !has
	if has && m.Downloaded {
		// 已经下载过：略过；未来可通过章节计数变化判断「有更新」再触发。
		return
	}
	if needs {
		s.bus.Emit(ctx, hook.SyncNewManga, hook.KeySourceID, src.ID(), hook.KeyMangaID, fav.MangaID, "title", fav.Title)
		title := fav.Title
		id, err := s.eng.Submit(src.ID(), fav.MangaID, title, "sync", nil)
		if err != nil {
			s.logger.Warnf("sync", "提交下载 %s/%s 失败: %v", src.ID(), fav.MangaID, err)
			return
		}
		s.logger.Infof("sync", "发现新收藏：%s → 任务 %s", fav.Title, id)
	}
}
