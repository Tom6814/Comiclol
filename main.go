// Command tsukimi 是禁漫个人漫画管理站的入口。
//
// 它把所有组件按依赖顺序拼起来：配置 → 存储 → 书库 → 会话 →
// 源注册表（禁漫）→ sink 注册表（本地）→ 下载引擎 → 同步服务 → HTTP 服务。
// 整个组装过程保持线性、显式，没有依赖注入框架——个人项目足够清晰就好。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tsukimi/internal/config"
	"tsukimi/internal/download"
	"tsukimi/internal/history"
	"tsukimi/internal/jmcomic"
	"tsukimi/internal/library"
	"tsukimi/internal/plugin"
	"tsukimi/internal/server"
	"tsukimi/internal/session"
	"tsukimi/internal/sink"
	localsink "tsukimi/internal/sink/local"
	"tsukimi/internal/source"
	"tsukimi/internal/store"
	"tsukimi/internal/syncfav"
)

func main() {
	var (
		configPath = flag.String("config", "", "配置文件路径（默认 $TSUKIMI_CONFIG，或 $DATA_DIR/config.json，不存在自动生成）")
		dataDir    = flag.String("data", "", "数据目录（默认 $TSUKIMI_DATA_DIR 或 $HOME/.tsukimi）")
		addr       = flag.String("addr", "", "HTTP 监听地址（默认 :7878；Zeabur 等 PaaS 平台自动使用 $PORT）")
	)
	flag.Parse()

	// 数据目录优先级：-data > $TSUKIMI_DATA_DIR > 配置默认
	// Zeabur 部署时把持久化 volume 挂到 /data 并设 TSUKIMI_DATA_DIR=/data。
	if *dataDir == "" {
		*dataDir = os.Getenv("TSUKIMI_DATA_DIR")
	}
	// 配置路径优先级：-config > $TSUKIMI_CONFIG > $DATA_DIR/config.json > ./config.json
	if *configPath == "" {
		*configPath = os.Getenv("TSUKIMI_CONFIG")
	}
	if *configPath == "" {
		if *dataDir != "" {
			*configPath = filepath.Join(*dataDir, "config.json")
		} else {
			*configPath = "config.json"
		}
	}

	logger := plugin.NewLogger()
	bus := plugin.NewBus(logger)
	ctx := plugin.NewContext(logger, bus)

	// 1) 配置
	cfg, err := config.Load(*configPath)
	must(logger, "load config", err)
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	// 监听地址优先级：-addr > $PORT（Zeabur 自动注入）> 配置
	if *addr != "" {
		cfg.Addr = *addr
	} else if p := os.Getenv("PORT"); p != "" {
		cfg.Addr = "0.0.0.0:" + p
	}
	logger.Infof("main", "数据目录：%s", cfg.DataDir)

	// 2) 存储、书库、会话
	st, err := store.New(cfg.DataDir)
	must(logger, "init store", err)

	lib, err := library.New(cfg.DataDir, st)
	must(logger, "init library", err)

	sess := session.New()

	// 3) 来源（禁漫）
	// 注意：jm.domains 配置的是网页域名（18comic.vip 等），仅供 HTML 实现使用；
	// APP API 实现有自己专属的域名池（cdnhjk.net 等），由 newAPIClient 内置，
	// 所以这里 APIDomains 留空，让它走内置默认值。
	srcReg := source.NewRegistry()
	// InsecureTLS：禁漫 CDN 证书状态不稳（历史上有过过期 / 自签），
	// 默认开启跳过校验；用户介意可设 TSUKIMI_VERIFY_TLS=1 强制校验。
	insecureTLS := os.Getenv("TSUKIMI_VERIFY_TLS") != "1"
	jm, err := jmcomic.New(jmcomic.Options{
		Impl:        readImpl(cfg),
		APIDomains:  nil,
		HTMLDomains: cfg.JM.Domains,
		ImageHost:   cfg.JM.ImageHost,
		Retry:       cfg.RetryTimes,
		Username:    cfg.JM.Username,
		Password:    cfg.JM.Password,
		Cookies:     parseCookie(cfg.JM.AVSCookie),
		InsecureTLS: insecureTLS,
	})
	must(logger, "init jmcomic", err)
	if err := srcReg.Register(jm); err != nil {
		must(logger, "register jmcomic", err)
	}
	logger.Infof("main", "已注册来源 jmcomic（impl=%s）", jm.Impl())

	// 4) Sink（本地）
	sinkReg := sink.NewRegistry()
	local := localsink.New()
	_ = local.Configure(map[string]any{"data_dir": cfg.DataDir})
	sinkReg.Register(local)

	// 5) 下载引擎
	// 启动时按磁盘实际文件校正「已下载」标记——重新部署后 /data 持久化了文件，
	// 但 library.json 的 Downloaded 可能失真；以磁盘为准。
	if n := lib.ReconcileDownloaded(); n > 0 {
		logger.Infof("main", "已按磁盘文件校正 %d 本漫画的下载状态", n)
	}
	// 任务级串行：一次只下载一个任务（Start(1)），其余排队等待；
	// 单任务内部多线程（章节并发 ChapterJobs、图片并发 Concurrency）。
	eng := download.NewEngine(srcReg, lib, bus, logger, sess, download.Options{
		ImageQuality: cfg.ImageQuality,
		ChapterJobs:  cfg.ChapterJobs,
		ImageWorkers: cfg.Concurrency,
		ImageRetries: cfg.RetryTimes,
	})
	eng.Start(1)

	// 6) 同步服务
	// 总是构造 Service（让前端「立即同步」随时可用），只在启用轮询时才 Start()。
	syncSvc := syncfav.New(srcReg, sess, lib, eng, bus, logger, time.Duration(cfg.SyncInterval)*time.Second, cfg.SyncRecentCount)
	if cfg.SyncEnabled && cfg.SyncInterval > 0 {
		syncSvc.Start(context.Background())
		logger.Infof("main", "同步服务已启用，间隔 %ds", cfg.SyncInterval)
	} else {
		logger.Infof("main", "自动轮询未启用（仍可手动触发同步）")
	}

	// 7) 阅读进度（服务端持久化，跨设备续读）
	hist, err := history.New(st)
	must(logger, "init history", err)

	// 8) HTTP 服务
	srv := server.New(cfg, lib, eng, syncSvc, hist, sess, srcReg, sinkReg, bus, logger)

	// 9) 启动 + 信号处理
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logger.Errorf("main", "HTTP 服务退出: %v", err)
		}
	}()

	// 让 plugin context 提前记录这次启动事件，方便前端订阅
	bus.Emit(context.Background(), "host.boot", "at", time.Now().Unix())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	logger.Infof("main", "收到信号 %s，开始退出", s)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	eng.Stop()
	if syncSvc != nil {
		syncSvc.Stop()
	}
	ctx.Dispose()
	logger.Infof("main", "已退出")
}

func must(logger *plugin.Logger, what string, err error) {
	if err != nil {
		logger.Errorf("main", "%s 失败: %v", what, err)
		os.Exit(1)
	}
}

// readImpl 从插件配置里读 jmcomic 的 impl（默认 api）。
func readImpl(cfg *config.Config) string {
	if cfg.Plugins != nil {
		if jm, ok := cfg.Plugins["jmcomic"]; ok {
			if v, ok := jm["impl"].(string); ok && (v == "api" || v == "html") {
				return v
			}
		}
	}
	return "api"
}

// parseCookie 把 "AVS=xxx; ORI=yyy" 形式的 cookie 字符串解析成 map。
func parseCookie(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range splitAndTrim(raw, ';') {
		kv := splitAndTrim(part, '=')
		if len(kv) == 2 && kv[0] != "" {
			out[kv[0]] = kv[1]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitAndTrim(s string, sep rune) []string {
	out := []string{}
	cur := []rune{}
	for _, r := range s {
		if r == sep {
			out = append(out, trimRunes(cur))
			cur = cur[:0]
		} else {
			cur = append(cur, r)
		}
	}
	out = append(out, trimRunes(cur))
	return out
}

func trimRunes(rs []rune) string {
	i, j := 0, len(rs)
	for i < j && (rs[i] == ' ' || rs[i] == '\t') {
		i++
	}
	for j > i && (rs[j-1] == ' ' || rs[j-1] == '\t') {
		j--
	}
	return string(rs[i:j])
}
