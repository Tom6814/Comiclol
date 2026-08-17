// Package httpclient 是禁漫专用的 HTTP 客户端。
//
// 特性：
//   - 持久 cookie jar（登录态）
//   - 多域名轮换：某个域名重试失败后切换下一个
//   - 固定 UA / Referer / Accept-Language 头，模拟网页端
//   - 可配置重试次数与超时
//   - 线程安全：每个请求独立，cookie jar 用锁保护
package httpclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	http      *http.Client
	domains   []string
	retry     int
	domainMu  sync.Mutex
	curDomain int
	cookies   map[string]string
	cookieMu  sync.RWMutex
	userAgent string
}

type Options struct {
	Domains   []string
	Retry     int
	Timeout   time.Duration
	UserAgent string
	Cookies   map[string]string
}

func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	}
	c := &Client{
		domains:   opts.Domains,
		retry:     opts.Retry,
		cookies:   map[string]string{},
		userAgent: opts.UserAgent,
		http: &http.Client{
			Timeout: opts.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// 跟随重定向但保留手动 cookie 注入
				return nil
			},
		},
	}
	if opts.Cookies != nil {
		for k, v := range opts.Cookies {
			c.cookies[k] = v
		}
	}
	if len(c.domains) == 0 {
		c.domains = []string{"18comic.vip"}
	}
	return c
}

// CurrentDomain 返回当前使用的域名。
func (c *Client) CurrentDomain() string {
	c.domainMu.Lock()
	defer c.domainMu.Unlock()
	if len(c.domains) == 0 {
		return ""
	}
	return c.domains[c.curDomain%len(c.domains)]
}

// SetCookies 覆盖 cookie（登录成功后调用）。
func (c *Client) SetCookies(cookies map[string]string) {
	c.cookieMu.Lock()
	c.cookies = map[string]string{}
	for k, v := range cookies {
		c.cookies[k] = v
	}
	c.cookieMu.Unlock()
}

// Cookies 返回当前 cookie 副本。
func (c *Client) Cookies() map[string]string {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	out := make(map[string]string, len(c.cookies))
	for k, v := range c.cookies {
		out[k] = v
	}
	return out
}

func (c *Client) cookieHeader() string {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	if len(c.cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c.cookies))
	for k, v := range c.cookies {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, "; ")
}

func (c *Client) applyHeaders(req *http.Request, isImage bool, refererDomain string) {
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	ck := c.cookieHeader()
	if ck != "" {
		req.Header.Set("Cookie", ck)
	}
	if isImage {
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	} else {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	}
	if refererDomain != "" {
		req.Header.Set("Referer", "https://"+refererDomain+"/")
	}
}

// GetPath 以路径（如 /album/123/）发起请求，自动补当前域名。
func (c *Client) GetPath(ctx context.Context, path string) (resp *Response, err error) {
	return c.doWithRetry(ctx, "GET", path, false, nil, "")
}

// GetURL 以完整 URL 发起请求（用于图片，不切换域名）。
func (c *Client) GetURL(ctx context.Context, url string, isImage bool, refererDomain string) (resp *Response, err error) {
	return c.doOnce(ctx, "GET", url, isImage, nil, refererDomain, true)
}

// PostPath 表单 POST。
func (c *Client) PostPath(ctx context.Context, path string, body io.Reader) (resp *Response, err error) {
	return c.doWithRetry(ctx, "POST", path, false, body, "")
}

func (c *Client) doWithRetry(ctx context.Context, method, path string, isImage bool, body io.Reader, refererDomain string) (*Response, error) {
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	domainIdx := 0
	c.domainMu.Lock()
	startDomain := c.curDomain
	c.domainMu.Unlock()
	domainIdx = startDomain

	var lastErr error
	for attempt := 0; attempt < c.retry+1; attempt++ {
		domain := c.domains[domainIdx%len(c.domains)]
		url := "https://" + domain + path
		r, err := c.doOnce(ctx, method, url, isImage, body, domain, false)
		if err == nil && r.StatusCode < 500 {
			return r, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d", r.StatusCode)
			r.Body.Close()
		}
		// 同域名重试到上限后切换域名
		if attempt < c.retry {
			continue
		}
		domainIdx++
		if domainIdx%len(c.domains) == startDomain%len(c.domains) && attempt >= c.retry {
			// 全部域名都试过
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed: %s", path)
	}
	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, url string, isImage bool, body io.Reader, refererDomain string, reuseCookies bool) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	c.applyHeaders(req, isImage, refererDomain)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	// 收集 Set-Cookie（登录后服务端下发 AVS）
	if !reuseCookies {
		for _, ck := range resp.Cookies() {
			c.cookieMu.Lock()
			c.cookies[ck.Name] = ck.Value
			c.cookieMu.Unlock()
		}
	}
	r := &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		body:       resp.Body,
		url:        url,
	}
	r.Body = &readCloser{inner: resp.Body, r: r}
	return r, nil
}

// Response 包装 http.Response，缓存已读 body 与最终 URL。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	body       io.ReadCloser
	url        string

	once  sync.Once
	data  []byte
	rerr  error
	final string
}

type readCloser struct {
	inner io.ReadCloser
	r     *Response
}

func (rc *readCloser) Read(p []byte) (int, error) { return rc.inner.Read(p) }
func (rc *readCloser) Close() error {
	return rc.inner.Close()
}

// Bytes 读取并缓存整个 body。
func (r *Response) Bytes() ([]byte, error) {
	r.once.Do(func() {
		if r.body == nil {
			return
		}
		b, err := io.ReadAll(r.body)
		r.data = b
		r.rerr = err
		r.body.Close()
	})
	return r.data, r.rerr
}

// Text 返回 body 字符串。
func (r *Response) Text() (string, error) {
	b, err := r.Bytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// URL 返回最终 URL（未跟随重定向时等于请求 URL）。
func (r *Response) URL() string { return r.url }
