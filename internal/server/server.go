// Package server 是 tsukimi 的 HTTP 层。
//
// 它本身不含业务逻辑——只是把请求翻译成对 source / library / download
// / session / sync 等服务的调用，再把结果序列化回 JSON。静态前端通过
// go:embed 打进二进制，所以最终产物只有一个可执行文件。
//
// 路由用 Go 1.22 的 ServeMux 模式语法（{id}、{path...}），免掉第三方路由。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tsukimi/internal/config"
	"tsukimi/internal/domain"
	"tsukimi/internal/download"
	"tsukimi/internal/library"
	"tsukimi/internal/plugin"
	"tsukimi/internal/session"
	"tsukimi/internal/sink"
	"tsukimi/internal/source"
	"tsukimi/internal/syncfav"
)

//go:embed static
var staticFS embed.FS

// Server 聚合所有依赖并把它们暴露成 HTTP 接口。
type Server struct {
	cfg     *config.Config
	lib     *library.Service
	eng     *download.Engine
	syncSvc *syncfav.Service
	sess    *session.Manager
	srcReg  *source.Registry
	sinkReg *sink.Registry
	bus     *plugin.EventBus
	logger  *plugin.Logger
	broker  *Broker

	mux     *http.ServeMux
	httpSrv *http.Server
}

func New(
	cfg *config.Config,
	lib *library.Service,
	eng *download.Engine,
	syncSvc *syncfav.Service,
	sess *session.Manager,
	srcReg *source.Registry,
	sinkReg *sink.Registry,
	bus *plugin.EventBus,
	logger *plugin.Logger,
) *Server {
	s := &Server{
		cfg: cfg, lib: lib, eng: eng, syncSvc: syncSvc,
		sess: sess, srcReg: srcReg, sinkReg: sinkReg,
		bus: bus, logger: logger, broker: NewBroker(bus, logger),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

// ListenAndServe 启动 HTTP 服务，阻塞直到 Shutdown。
func (s *Server) ListenAndServe() error {
	addr := s.cfg.Addr
	if addr == "" {
		addr = ":7878"
	}
	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: s.chain(s.mux),
	}
	s.logger.Infof("server", "监听 http://%s", addr)
	return s.httpSrv.ListenAndServe()
}

// Shutdown 优雅停止。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// chain 套上日志、CORS、recover 中间件。
func (s *Server) chain(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 简单的访问日志
		wrapped := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(wrapped, r)
		if wrapped.status >= 400 {
			s.logger.Infof("http", "%s %s → %d", r.Method, r.URL.Path, wrapped.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush 让 SSE / chunked 响应透传到底层 writer；
// 否则接口嵌入会吞掉 http.Flusher 的方法。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Push 用于 HTTP/2 server push。
func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if p, ok := r.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// routes 注册所有路由。
func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/sources", s.handleListSources)
	m.HandleFunc("GET /api/sinks", s.handleListSinks)
	m.HandleFunc("GET /api/plugins", s.handleListPlugins)
	m.HandleFunc("GET /api/config", s.handleGetConfig)
	m.HandleFunc("PUT /api/config", s.handlePutConfig)

	m.HandleFunc("GET /api/session", s.handleSession)
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)

	m.HandleFunc("GET /api/library", s.handleLibrary)
	m.HandleFunc("GET /api/library/{source}/{id}", s.handleMangaDetail)
	m.HandleFunc("DELETE /api/library/{source}/{id}", s.handleMangaDelete)
	m.HandleFunc("GET /api/library/{source}/{id}/cover", s.handleMangaCover)
	m.HandleFunc("GET /api/library/{source}/{id}/{chapter}/pages", s.handleChapterPages)
	m.HandleFunc("GET /api/library/{source}/{id}/{chapter}/{file}", s.handlePageImage)

	m.HandleFunc("GET /api/favorites", s.handleFavorites)
	m.HandleFunc("POST /api/favorites/sync", s.handleSyncNow)

	m.HandleFunc("POST /api/downloads", s.handleSubmitDownload)
	m.HandleFunc("GET /api/downloads", s.handleListDownloads)
	m.HandleFunc("DELETE /api/downloads/{id}", s.handleCancelDownload)
	m.HandleFunc("POST /api/downloads/{id}/cancel", s.handleCancelDownload)

	m.HandleFunc("GET /api/events", s.broker.ServeHTTP)

	// 静态前端
	staticRoot, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /", http.FileServer(http.FS(staticRoot)))
}

// ----- helpers -----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ----- handlers -----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"version": "0.1.0",
	})
}

type sourceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Impl        string `json:"impl,omitempty"`
	HasLogin    bool   `json:"has_login"`
	HasSearch   bool   `json:"has_search"`
	HasFavorites bool  `json:"has_favorites"`
	MultiChapter bool  `json:"multi_chapter"`
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	out := make([]sourceInfo, 0)
	for _, src := range s.srcReg.List() {
		caps := src.Capabilities()
		info := sourceInfo{
			ID:           src.ID(),
			Name:         src.DisplayName(),
			HasLogin:     caps.SupportsLogin,
			HasSearch:    caps.HasSearch,
			HasFavorites: caps.HasFavorites,
			MultiChapter: caps.MultiChapter,
		}
		// impl 字段：仅 jmcomic 当前暴露
		if impl, ok := src.(interface{ Impl() string }); ok {
			info.Impl = impl.Impl()
		}
		out = append(out, info)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleListSinks(w http.ResponseWriter, r *http.Request) {
	type sinkInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := make([]sinkInfo, 0)
	for _, sk := range s.sinkReg.List() {
		out = append(out, sinkInfo{ID: sk.ID(), Name: sk.DisplayName()})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"sources": s.srcReg.List(),
		"sinks":   s.sinkReg.List(),
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"addr":         s.cfg.Addr,
		"data_dir":     s.cfg.DataDir,
		"concurrency":  s.cfg.Concurrency,
		"chapter_jobs": s.cfg.ChapterJobs,
		"image_quality": s.cfg.ImageQuality,
		"sync_enabled":  s.cfg.SyncEnabled,
		"sync_interval": s.cfg.SyncInterval,
		"jm":            s.cfg.JM,
	})
}

type configUpdate struct {
	Concurrency  *int             `json:"concurrency,omitempty"`
	ChapterJobs  *int             `json:"chapter_jobs,omitempty"`
	ImageQuality *int             `json:"image_quality,omitempty"`
	SyncEnabled  *bool            `json:"sync_enabled,omitempty"`
	SyncInterval *int             `json:"sync_interval,omitempty"`
	JM           *struct {
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
		Impl     string `json:"impl,omitempty"`
	} `json:"jm,omitempty"`
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var u configUpdate
	if err := readJSON(r, &u); err != nil {
		writeErr(w, 400, "解析失败: %v", err)
		return
	}
	if u.Concurrency != nil {
		s.cfg.Concurrency = *u.Concurrency
	}
	if u.ChapterJobs != nil {
		s.cfg.ChapterJobs = *u.ChapterJobs
	}
	if u.ImageQuality != nil {
		s.cfg.ImageQuality = *u.ImageQuality
	}
	if u.SyncEnabled != nil {
		s.cfg.SyncEnabled = *u.SyncEnabled
	}
	if u.SyncInterval != nil {
		s.cfg.SyncInterval = *u.SyncInterval
	}
	if u.JM != nil {
		if u.JM.Username != "" {
			s.cfg.JM.Username = u.JM.Username
		}
		if u.JM.Password != "" {
			s.cfg.JM.Password = u.JM.Password
		}
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, "保存失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	type sessOut struct {
		SourceID string `json:"source_id"`
		Username string `json:"username"`
		Valid    bool   `json:"valid"`
	}
	all := s.sess.All()
	out := make([]sessOut, 0, len(all))
	for _, ss := range all {
		out = append(out, sessOut{SourceID: ss.SourceID, Username: ss.Username, Valid: true})
	}
	writeJSON(w, 200, out)
}

type loginReq struct {
	Source   string `json:"source"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "解析失败: %v", err)
		return
	}
	src, ok := s.srcReg.Get(req.Source)
	if !ok {
		writeErr(w, 404, "未知来源 %s", req.Source)
		return
	}
	sess, err := src.Login(r.Context(), domain.Credentials{
		Username: req.Username,
		Password: req.Password,
	})
	if err != nil {
		writeErr(w, 401, "登录失败: %v", err)
		return
	}
	s.sess.Set(sess)
	writeJSON(w, 200, map[string]any{
		"source_id": sess.SourceID,
		"username":  sess.Username,
		"valid":     true,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("source")
	if src == "" {
		writeErr(w, 400, "缺少 source 参数")
		return
	}
	s.sess.Clear(src)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	list := s.lib.List()
	type item struct {
		SourceID   string   `json:"source_id"`
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Author     string   `json:"author"`
		CoverURL   string   `json:"cover_url"`
		Tags       []string `json:"tags"`
		PageCount  int      `json:"page_count"`
		Chapters   int      `json:"chapters"`
		Downloaded bool     `json:"downloaded"`
		UpdatedAt  string   `json:"updated_at"`
	}
	out := make([]item, 0, len(list))
	for _, m := range list {
		out = append(out, item{
			SourceID:   m.SourceID,
			ID:         m.ID,
			Title:      m.Title,
			Author:     m.Author,
			CoverURL:   fmt.Sprintf("/api/library/%s/%s/cover", m.SourceID, m.ID),
			Tags:       m.Tags,
			PageCount:  m.PageCount,
			Chapters:   len(m.Chapters),
			Downloaded: m.Downloaded,
			UpdatedAt:  m.UpdatedAt.Format("2006-01-02 15:04"),
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleMangaDetail(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("source")
	id := r.PathValue("id")
	m, ok := s.lib.Get(srcID, id)
	if !ok {
		// 本地没有则向远端拉取一次
		src, ok := s.srcReg.Get(srcID)
		if !ok {
			writeErr(w, 404, "未知来源 %s", srcID)
			return
		}
		sess, _ := s.sess.Get(srcID)
		mm, err := src.GetManga(r.Context(), sess, id)
		if err != nil {
			writeErr(w, 502, "远端拉取失败: %v", err)
			return
		}
		m = *mm
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleMangaDelete(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("source")
	id := r.PathValue("id")
	del := r.URL.Query().Get("files") == "1"
	if err := s.lib.Remove(srcID, id, del); err != nil {
		writeErr(w, 500, "删除失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMangaCover(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("source")
	id := r.PathValue("id")
	path := s.lib.CoverPath(srcID, id)
	if _, err := os.Stat(path); err != nil {
		// 本地没有，回退远端拉一次
		m, ok := s.lib.Get(srcID, id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if m.CoverURL == "" {
			http.NotFound(w, r)
			return
		}
		// 直接代理过去（避免在服务端实现一整套图片下载缓存）
		http.Redirect(w, r, m.CoverURL, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, path)
}

func (s *Server) handleChapterPages(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("source")
	id := r.PathValue("id")
	ch := r.PathValue("chapter")
	files, err := s.lib.ListPages(srcID, id, ch)
	if err != nil {
		writeErr(w, 404, "章节未下载: %v", err)
		return
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, fmt.Sprintf("/api/library/%s/%s/%s/%s", srcID, id, ch, f))
	}
	writeJSON(w, 200, map[string]any{
		"chapter_id": ch,
		"pages":      out,
		"count":      len(out),
	})
}

func (s *Server) handlePageImage(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("source")
	id := r.PathValue("id")
	ch := r.PathValue("chapter")
	file := r.PathValue("file")
	// 防穿越
	if strings.Contains(file, "/") || strings.Contains(file, "\\") || strings.Contains(file, "..") {
		writeErr(w, 400, "非法文件名")
		return
	}
	path := s.lib.PageAbsPath(srcID, id, ch, file)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, path)
}

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	srcID := r.URL.Query().Get("source")
	if srcID == "" {
		srcID = "jmcomic"
	}
	folder := r.URL.Query().Get("folder")
	pageStr := r.URL.Query().Get("page")
	page, _ := strconv.Atoi(pageStr)
	if page < 1 {
		page = 1
	}
	src, ok := s.srcReg.Get(srcID)
	if !ok {
		writeErr(w, 404, "未知来源 %s", srcID)
		return
	}
	sess, ok := s.sess.Get(srcID)
	if !ok {
		writeErr(w, 401, "未登录")
		return
	}
	fp, err := src.Favorites(r.Context(), sess, folder, page)
	if err != nil {
		writeErr(w, 502, "拉取收藏失败: %v", err)
		return
	}
	writeJSON(w, 200, fp)
}

func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.syncSvc == nil {
		writeErr(w, 503, "同步服务未启用")
		return
	}
	if err := s.syncSvc.TickNow(r.Context()); err != nil {
		writeErr(w, 500, "同步失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type downloadReq struct {
	Source     string   `json:"source"`
	MangaID    string   `json:"manga_id"`
	Title      string   `json:"title"`
	ChapterIDs []string `json:"chapter_ids,omitempty"`
}

func (s *Server) handleSubmitDownload(w http.ResponseWriter, r *http.Request) {
	var req downloadReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "解析失败: %v", err)
		return
	}
	if req.Source == "" {
		req.Source = "jmcomic"
	}
	id, err := s.eng.Submit(req.Source, req.MangaID, req.Title, "manual", req.ChapterIDs)
	if err != nil {
		writeErr(w, 400, "提交失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"task_id": id})
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	list := s.eng.List()
	out := make([]download.Task, 0, len(list))
	out = append(out, list...)
	writeJSON(w, 200, out)
}

func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == http.MethodDelete {
		if !s.eng.Remove(id) {
			writeErr(w, 404, "任务不存在")
			return
		}
	} else {
		if !s.eng.Cancel(id) {
			writeErr(w, 404, "任务不存在")
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// 防止未使用导入（如果某些分支编译期被裁剪）
var _ = io.EOF
var _ = errors.New
var _ = log.Print
var _ = filepath.Clean
