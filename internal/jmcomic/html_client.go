package jmcomic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"tsukimi/internal/domain"
)

type htmlClientConfig struct {
	HTMLDomains []string
	ImageHost   string
	Retry       int
	Cookies     map[string]string
}

// htmlClient 是网页端 HTML 实现（兜底方案）。
//
// 登录走 POST /login 表单换取 AVS；album/photo/favorites 通过正则解析 HTML。
// 图片去混淆由 internal/img 统一处理。
type htmlClient struct {
	http     *http.Client
	domains  []string
	imgHost  string
	retry    int
	mu       sync.RWMutex
	cookies  map[string]string
	username string
}

func newHTMLClient(cfg htmlClientConfig) *htmlClient {
	domains := cfg.HTMLDomains
	if len(domains) == 0 {
		domains = []string{"18comic.vip", "18comic.org", "jm-comic.club"}
	}
	c := &htmlClient{
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return nil
			},
		},
		domains: domains,
		imgHost: cfg.ImageHost,
		retry:   cfg.Retry,
		cookies: map[string]string{},
	}
	if c.imgHost == "" {
		c.imgHost = "cdn-msp2.18comic.org"
	}
	if c.retry <= 0 {
		c.retry = 5
	}
	if cfg.Cookies != nil {
		for k, v := range cfg.Cookies {
			c.cookies[k] = v
		}
	}
	return c
}

func (c *htmlClient) ImplKey() string { return "html" }

func (c *htmlClient) setCookies(cookies map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range cookies {
		c.cookies[k] = v
	}
}

func (c *htmlClient) snapshotCookies() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c.cookies))
	for k, v := range c.cookies {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, "; ")
}

func (c *htmlClient) currentDomain() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.domains) == 0 {
		return "18comic.vip"
	}
	return c.domains[0]
}

// ---- Login ----

func (c *htmlClient) Login(ctx context.Context, sess domain.Session, username, password string) (domain.Session, error) {
	form := fmt.Sprintf("username=%s&password=%s&id_remember=on&login_remember=on&submit_login=",
		urlEncode(username), urlEncode(password))
	resp, err := c.doPath(ctx, "POST", "/login", bytes.NewReader([]byte(form)), true)
	if err != nil {
		return domain.Session{}, fmt.Errorf("登录请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return domain.Session{}, fmt.Errorf("登录失败，状态码 %d", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		c.mu.Lock()
		c.cookies[ck.Name] = ck.Value
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.username = username
	cookies := map[string]string{}
	for k, v := range c.cookies {
		cookies[k] = v
	}
	c.mu.Unlock()
	if cookies["AVS"] == "" {
		return domain.Session{}, errors.New("登录未返回 AVS cookie，请检查账号密码或域名")
	}
	return domain.Session{
		SourceID:   SourceID,
		Username:   username,
		Cookies:    cookies,
		ValidUntil: futureDuration(days14),
	}, nil
}

// ---- Album ----

func (c *htmlClient) FetchAlbum(ctx context.Context, sess domain.Session, albumID string) (*domain.Manga, error) {
	c.injectSession(sess)
	html, err := c.fetchHTML(ctx, "/album/"+albumID+"/")
	if err != nil {
		return nil, err
	}
	return parseAlbumHTML(html, c.albumCoverURL(albumID))
}

// ---- Chapter ----

func (c *htmlClient) FetchChapter(ctx context.Context, sess domain.Session, chapterID string) (*domain.Chapter, []domain.Page, error) {
	c.injectSession(sess)
	html, err := c.fetchHTML(ctx, "/photo/"+chapterID+"/")
	if err != nil {
		return nil, nil, err
	}
	ch, pages, err := parsePhotoHTML(html, chapterID, c.currentDomain())
	if err != nil {
		return nil, nil, err
	}
	ch.ID = chapterID
	return ch, pages, nil
}

// ---- Favorites ----

func (c *htmlClient) FetchFavorites(ctx context.Context, sess domain.Session, folderID string, page int) (*domain.FavoritePage, error) {
	if sess.Username == "" {
		c.mu.RLock()
		uname := c.username
		c.mu.RUnlock()
		if uname == "" {
			return nil, errors.New("收藏夹需要先登录")
		}
		sess.Username = uname
	}
	c.injectSession(sess)
	path := fmt.Sprintf("/user/%s/favorite/albums?o=mr&page=%d", sess.Username, page)
	if folderID != "" && folderID != "0" {
		path = fmt.Sprintf("/user/%s/favorite/albums?folder=%s&o=mr&page=%d", sess.Username, folderID, page)
	}
	html, err := c.fetchHTML(ctx, path)
	if err != nil {
		return nil, err
	}
	return parseFavoritesHTML(html, folderID, page)
}

// ---- Image ----

func (c *htmlClient) FetchImage(ctx context.Context, sess domain.Session, pg domain.Page) (domain.ImageData, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", pg.URL, nil)
	if err != nil {
		return domain.ImageData{}, err
	}
	req.Header.Set("User-Agent", htmlUA)
	req.Header.Set("Accept", "image/avif,image/webp,image/*,*/*;q=0.8")
	req.Header.Set("Referer", "https://"+c.currentDomain()+"/")
	if ck := c.snapshotCookies(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ImageData{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return domain.ImageData{}, err
	}
	if resp.StatusCode != 200 {
		return domain.ImageData{}, fmt.Errorf("图片请求失败 %d: %s", resp.StatusCode, pg.URL)
	}
	return domain.ImageData{
		Reader:      data,
		URL:         pg.URL,
		Suffix:      guessSuffix(pg.URL),
		ScrambleID:  pg.ScrambleID,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// ---- 内部 ----

func (c *htmlClient) injectSession(sess domain.Session) {
	if sess.Cookies == nil {
		return
	}
	c.mu.Lock()
	for k, v := range sess.Cookies {
		if v != "" {
			c.cookies[k] = v
		}
	}
	c.mu.Unlock()
}

func (c *htmlClient) fetchHTML(ctx context.Context, path string) (string, error) {
	resp, err := c.doPath(ctx, "GET", path, nil, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("请求 %s 失败: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	text := string(body)
	if b64 := matchGroup(htmlB64ContentRE, text, 1); b64 != "" {
		if decoded, derr := base64Decode(b64); derr == nil {
			text = decoded
		}
	}
	return text, nil
}

func (c *htmlClient) doPath(ctx context.Context, method, path string, body io.Reader, isLogin bool) (*http.Response, error) {
	var lastErr error
	for di := 0; di < len(c.domains); di++ {
		for attempt := 0; attempt <= c.retry; attempt++ {
			domain := c.domains[di%len(c.domains)]
			urlStr := "https://" + domain + path
			// body 可能已被消费，这里只在第一次需要；重试时如果是 POST 且 body 已读，需重新构造
			// 登录场景：重新构造 form
			var reqBody io.Reader
			if body != nil {
				if b, ok := body.(interface{ Bytes() []byte }); ok {
					reqBody = bytes.NewReader(b.Bytes())
				} else {
					// 退化：只在第一次用
					if attempt == 0 {
						reqBody = body
					}
				}
			}
			req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
			if err != nil {
				return nil, err
			}
			if method == "POST" {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			req.Header.Set("User-Agent", htmlUA)
			req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
			req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
			req.Header.Set("Referer", "https://"+domain+"/")
			if ck := c.snapshotCookies(); ck != "" {
				req.Header.Set("Cookie", ck)
			}
			resp, err := c.http.Do(req)
			if err == nil && resp.StatusCode < 500 {
				if !isLogin {
					for _, ck := range resp.Cookies() {
						c.mu.Lock()
						c.cookies[ck.Name] = ck.Value
						c.mu.Unlock()
					}
				}
				return resp, nil
			}
			if resp != nil {
				resp.Body.Close()
			}
			lastErr = err
			if err == nil {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("request failed")
	}
	return nil, lastErr
}

func (c *htmlClient) albumCoverURL(albumID string) string {
	return fmt.Sprintf("https://%s/media/albums/%s.jpg", c.imgHost, albumID)
}

const htmlUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
