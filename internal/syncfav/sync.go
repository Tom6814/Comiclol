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
	"sync"
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

	mu           sync.Mutex
	enabled      bool          // 当前是否启用轮询
	interval     time.Duration // 当前轮询间隔
	recentCount  int           // 每次最多处理最近 N 本收藏，0 = 不限
	cancel       context.CancelFunc
}

// New 构造一个同步服务。enabled=false 或 interval<=0 时不启动轮询，
// 但 Service 始终可用于手动 TickNow。
func New(srcReg *source.Registry, sess *session.Manager, lib *library.Service, eng *download.Engine, bus *plugin.EventBus, logger *plugin.Logger, interval time.Duration, recentCount int) *Service {
	enabled := interval > 0
	return &Service{
		srcReg:      srcReg,
		sess:        sess,
		lib:         lib,
		eng:         eng,
		bus:         bus,
		logger:      logger,
		enabled:     enabled,
		interval:    interval,
		recentCount: recentCount,
	}
}

// Start 启动后台轮询 goroutine（若已启用）。
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked(ctx)
}

// startLocked 调用方必须持有 s.mu。
func (s *Service) startLocked(ctx context.Context) {
	if !s.enabled || s.interval <= 0 {
		return
	}
	if s.cancel != nil {
		// 已在跑：先停掉旧的再重启，避免泄漏 goroutine。
		s.cancel()
		s.cancel = nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.loop(ctx)
}

// Stop 取消轮询。
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// UpdateConfig 热更新轮询配置，无需重启进程。
//   - enabled=false：停止轮询（手动 TickNow 仍可用）。
//   - interval 变化：自动重启 ticker。
//   - recentCount 变化：下次 tick 生效。
//
// 返回是否实际发生了变更（用于决定是否写日志）。
func (s *Service) UpdateConfig(ctx context.Context, enabled bool, interval time.Duration, recentCount int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.enabled != enabled || s.interval != interval || s.recentCount != recentCount
	s.enabled = enabled
	s.interval = interval
	s.recentCount = recentCount
	if !enabled || interval <= 0 {
		// 关闭轮询
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		return changed
	}
	// 启用或间隔变化：重启 ticker
	s.startLocked(ctx)
	return changed
}

func (s *Service) loop(ctx context.Context) {
	// 启动后先等一个 interval 再首次拉取，避免与启动期其它 IO 抢资源。
	s.mu.Lock()
	interval := s.interval
	s.mu.Unlock()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
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
	s.mu.Lock()
	recentCount := s.recentCount
	s.mu.Unlock()
	// recentCount>0：收藏列表按收藏时间倒序，前 N 本即「最近 N 本」。
	// 按列表顺序逐条处理（已下载的跳过），累计见到 N 本即停止翻页。
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
		seen := 0 // 已遍历的「最近 N 本」计数
		stop := recentCount > 0
		for {
			fp, err := src.Favorites(ctx, sess, folderID, page)
			if err != nil {
				s.logger.Warnf("sync", "%s 收藏第 %d 页失败: %v", src.ID(), page, err)
				break
			}
			for _, it := range fp.Items {
				s.maybeSync(ctx, src, it)
				if stop {
					seen++
					if seen >= recentCount {
						break
					}
				}
			}
			if stop && seen >= recentCount {
				break
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
		// 已经下载过：略过。
		return
	}
	if needs {
		s.bus.Emit(ctx, hook.SyncNewManga, hook.KeySourceID, src.ID(), hook.KeyMangaID, fav.MangaID, "title", fav.Title)
		id, err := s.eng.Submit(src.ID(), fav.MangaID, fav.Title, "sync", nil)
		if err != nil {
			s.logger.Warnf("sync", "提交下载 %s/%s 失败: %v", src.ID(), fav.MangaID, err)
			return
		}
		s.logger.Infof("sync", "发现新收藏：%s → 任务 %s", fav.Title, id)
	}
}
