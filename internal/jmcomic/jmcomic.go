// Package jmcomic 把禁漫天堂实现为一个 source.Source 插件。
//
// 架构：Plugin 实现 source.Source，内部把网络/解析细节委托给可插拔的 Client。
// 当前提供两种 Client 实现：
//
//   - apiClient：APP 移动端 API（默认）。结构化 JSON，不限 IP，需加解密。
//   - htmlClient：网页端 HTML。解析脆弱，限 IP 地区，但实现简单、无需加解密。
//
// 通过配置 jm.impl = "api" | "html" 切换。这呼应参考项目 html/api 双实现，
// 也落实了「万物皆插件」——传输层本身也是一种可替换组件。
package jmcomic

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"tsukimi/internal/domain"
	"tsukimi/internal/source"
)

const SourceID = "jmcomic"

// Client 是传输层的抽象，html 和 api 各是一种实现。
type Client interface {
	ImplKey() string
	Login(ctx context.Context, sess domain.Session, username, password string) (domain.Session, error)
	FetchAlbum(ctx context.Context, sess domain.Session, albumID string) (*domain.Manga, error)
	FetchChapter(ctx context.Context, sess domain.Session, chapterID string) (*domain.Chapter, []domain.Page, error)
	FetchFavorites(ctx context.Context, sess domain.Session, folderID string, page int) (*domain.FavoritePage, error)
	FetchImage(ctx context.Context, sess domain.Session, page domain.Page) (domain.ImageData, error)
}

// Plugin 是禁漫源插件的载体。
type Plugin struct {
	mu        sync.RWMutex
	impl      string // "api" | "html"
	client    Client
	username  string
	password  string
	imageHost string
}

// Options 构造一个插件。
type Options struct {
	Impl      string            // "api"（默认）| "html"
	APIDomains []string         // APP API 域名列表（api 实现）
	HTMLDomains []string        // 网页域名列表（html 实现）
	ImageHost string            // 图片 CDN 域名
	ImageHosts []string         // 图片 CDN 域名池（轮询）
	Retry     int
	Username  string
	Password  string
	Cookies   map[string]string // AVS 等
}

func New(opts Options) (*Plugin, error) {
	if opts.Impl == "" {
		opts.Impl = "api"
	}
	p := &Plugin{
		impl:      opts.Impl,
		username:  opts.Username,
		password:  opts.Password,
		imageHost: opts.ImageHost,
	}
	if p.imageHost == "" {
		if len(opts.ImageHosts) > 0 {
			p.imageHost = opts.ImageHosts[0]
		} else {
			p.imageHost = "cdn-msp2.jmapiproxy2.cc"
		}
	}

	switch opts.Impl {
	case "api", "":
		c, err := newAPIClient(apiClientConfig{
			APIDomains: opts.APIDomains,
			ImageHosts: orFallback(opts.ImageHosts, []string{p.imageHost}),
			Retry:      opts.Retry,
			Cookies:    opts.Cookies,
		})
		if err != nil {
			return nil, err
		}
		p.client = c
	case "html":
		p.client = newHTMLClient(htmlClientConfig{
			HTMLDomains: opts.HTMLDomains,
			ImageHost:   p.imageHost,
			Retry:       opts.Retry,
			Cookies:     opts.Cookies,
		})
	default:
		return nil, fmt.Errorf("未知 impl: %s（支持 api / html）", opts.Impl)
	}
	return p, nil
}

// Configure 更新凭据（来自配置或登录表单）。
func (p *Plugin) Configure(username, password string, cookies map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.username = username
	p.password = password
	if c, ok := p.client.(*apiClient); ok && cookies != nil {
		c.setCookies(cookies)
	}
	if c, ok := p.client.(*htmlClient); ok && cookies != nil {
		c.setCookies(cookies)
	}
}

func (p *Plugin) ID() string          { return SourceID }
func (p *Plugin) DisplayName() string { return "禁漫天堂" }
func (p *Plugin) Impl() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.impl
}

func (p *Plugin) Capabilities() source.Capabilities {
	return source.Capabilities{
		HasFavorites:     true,
		HasSearch:        true,
		SupportsLogin:    true,
		MultiChapter:     true,
		NeedsImageDecode: true,
	}
}

// Login 实现 source.Source。
func (p *Plugin) Login(ctx context.Context, creds domain.Credentials) (domain.Session, error) {
	p.mu.RLock()
	uname := creds.Username
	if uname == "" {
		uname = p.username
	}
	pw := creds.Password
	if pw == "" {
		pw = p.password
	}
	client := p.client
	p.mu.RUnlock()

	// 若已有有效 cookie，直接构造会话（免重复登录）
	if creds.Cookies != nil && creds.Cookies["AVS"] != "" {
		sess := domain.Session{
			SourceID:   SourceID,
			Username:   uname,
			Cookies:    creds.Cookies,
			ValidUntil: futureDuration(days14),
		}
		return sess, nil
	}

	if uname == "" || pw == "" {
		return domain.Session{}, errors.New("缺少用户名或密码")
	}
	sess, err := client.Login(ctx, domain.Session{SourceID: SourceID}, uname, pw)
	if err != nil {
		return domain.Session{}, err
	}
	if sess.Username == "" {
		sess.Username = uname
	}
	return sess, nil
}

// GetManga 抓取 album 详情。
func (p *Plugin) GetManga(ctx context.Context, sess domain.Session, mangaID string) (*domain.Manga, error) {
	mangaID = parseJMID(mangaID)
	p.mu.RLock()
	c := p.client
	host := p.imageHost
	p.mu.RUnlock()
	m, err := c.FetchAlbum(ctx, sess, mangaID)
	if err != nil {
		return nil, err
	}
	if m.CoverURL == "" {
		m.CoverURL = fmt.Sprintf("https://%s/media/albums/%s.jpg", host, mangaID)
	}
	return m, nil
}

// GetChapter 抓取 photo 详情。
func (p *Plugin) GetChapter(ctx context.Context, sess domain.Session, chapterID string) (*domain.Chapter, []domain.Page, error) {
	chapterID = parseJMID(chapterID)
	p.mu.RLock()
	c := p.client
	p.mu.RUnlock()
	return c.FetchChapter(ctx, sess, chapterID)
}

// Favorites 抓取收藏夹。
func (p *Plugin) Favorites(ctx context.Context, sess domain.Session, folderID string, page int) (*domain.FavoritePage, error) {
	if page < 1 {
		page = 1
	}
	p.mu.RLock()
	c := p.client
	p.mu.RUnlock()
	return c.FetchFavorites(ctx, sess, folderID, page)
}

// FavoriteFolders 列出收藏夹分类。
func (p *Plugin) FavoriteFolders(ctx context.Context, sess domain.Session) ([]domain.Folder, error) {
	return []domain.Folder{{ID: "0", Name: "全部", Count: 0}}, nil
}

// FetchImage 下载单张原始图片字节。
func (p *Plugin) FetchImage(ctx context.Context, sess domain.Session, pg domain.Page) (domain.ImageData, error) {
	p.mu.RLock()
	c := p.client
	p.mu.RUnlock()
	return c.FetchImage(ctx, sess, pg)
}

// 辅助
func orFallback(list, fallback []string) []string {
	if len(list) == 0 {
		return fallback
	}
	return list
}
