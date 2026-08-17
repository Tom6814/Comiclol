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
	"syscall"
	"time"

	"tsukimi/internal/config"
	"tsukimi/internal/download"
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
		configPath = flag.String("config", "config.json", "配置文件路径")
		dataDir    = flag.String("data", "", "数据目录（默认 $HOME/.tsukimi）")
		addr       = flag.String("addr", "", "HTTP 监听地址（覆盖配置）")
	)
	flag.Parse()

	logger := plugin.NewLogger()
	bus := plugin.NewBus(logger)
	ctx := plugin.NewContext(logger, bus)

	// 1) 配置
	cfg, err := config.Load(*configPath)
	must(logger, "load config", err)
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	logger.Infof("main", "数据目录：%s", cfg.DataDir)

	// 2) 存储、书库、会话
	st, err := store.New(cfg.DataDir)
	must(logger, "init store", err)

	lib, err := library.New(cfg.DataDir, st)
	must(logger, "init library", err)

	sess := session.New()

	// 3) 来源（禁漫）
	srcReg := source.NewRegistry()
	jm, err := jmcomic.New(jmcomic.Options{
		Impl:        readImpl(cfg),
		APIDomains:  cfg.JM.Domains,
		HTMLDomains: cfg.JM.Domains,
		ImageHost:   cfg.JM.ImageHost,
		Retry:       cfg.RetryTimes,
		Username:    cfg.JM.Username,
		Password:    cfg.JM.Password,
		Cookies:     parseCookie(cfg.JM.AVSCookie),
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
	eng := download.NewEngine(srcReg, lib, bus, logger, sess, download.Options{
		ImageQuality: cfg.ImageQuality,
	})
	eng.Start(cfg.ChapterJobs)

	// 6) 同步服务
	var syncSvc *syncfav.Service
	if cfg.SyncEnabled && cfg.SyncInterval > 0 {
		syncSvc = syncfav.New(srcReg, sess, lib, eng, bus, logger, time.Duration(cfg.SyncInterval)*time.Second)
		syncSvc.Start(context.Background())
		logger.Infof("main", "同步服务已启用，间隔 %ds", cfg.SyncInterval)
	} else {
		logger.Infof("main", "同步服务未启用（sync_enabled=false 或 sync_interval<=0）")
	}

	// 7) HTTP 服务
	srv := server.New(cfg, lib, eng, syncSvc, sess, srcReg, sinkReg, bus, logger)

	// 8) 启动 + 信号处理
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
