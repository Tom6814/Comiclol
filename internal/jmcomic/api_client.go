package jmcomic

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"tsukimi/internal/domain"
)

// APP API 接口路径（来自 jmcomic.JmApiClient）。
const (
	apiPathAlbum              = "/album"
	apiPathChapter            = "/chapter"
	apiPathChapterViewTpl     = "/chapter_view_template"
	apiPathLogin              = "/login"
	apiPathFavorite           = "/favorite"
	apiPathSearch             = "/search"
	apiPathSetting            = "/setting"
)

// APP API 域名池（来自 jm_config.DOMAIN_API_LIST）。
var defaultAPIDomains = []string{
	"www.cdnhjk.net",
	"www.cdngwc.cc",
	"www.cdngwc.net",
	"www.cdngwc.club",
}

// 图片 CDN 域名池（来自 jm_config.DOMAIN_IMAGE_LIST）。
var defaultImageDomains = []string{
	"cdn-msp.jmapiproxy1.cc",
	"cdn-msp.jmapiproxy2.cc",
	"cdn-msp2.jmapiproxy2.cc",
	"cdn-msp3.jmapiproxy2.cc",
	"cdn-msp.jmapinodeudzn.net",
	"cdn-msp3.jmapinodeudzn.net",
}

const appUA = "Mozilla/5.0 (Linux; Android 9; V1938CT Build/PQ3A.190705.11211812; wv) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/91.0.4472.114 Safari/537.36"

type apiClientConfig struct {
	APIDomains  []string
	ImageHosts  []string
	Retry       int
	Cookies     map[string]string
	InsecureTLS bool
}

// apiClient 是 APP 移动端 API 实现。
type apiClient struct {
	http     *http.Client
	domains  []string
	imgHosts []string
	retry    int

	mu         sync.RWMutex
	cookies    map[string]string
	imgIdx     uint32
	cookieOnce sync.Once

	// 进程级固定 ts/token（对齐 FLAG_USE_FIX_TIMESTAMP）
}

func newAPIClient(cfg apiClientConfig) (*apiClient, error) {
	domains := cfg.APIDomains
	if len(domains) == 0 {
		domains = defaultAPIDomains
	}
	imgHosts := cfg.ImageHosts
	if len(imgHosts) == 0 {
		imgHosts = defaultImageDomains
	}
	// 关键：必须用 http.DefaultTransport.Clone() 作为基础，而不是 &http.Transport{}。
	// 零值 Transport 缺失 DialContext/TLSHandshakeTimeout/ExpectContinueTimeout 等
	// 默认设置，在某些网络环境下会卡死在拨号阶段。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.IdleConnTimeout = 60 * time.Second
	if cfg.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不自动跟跳
		},
	}
	c := &apiClient{
		http:     httpClient,
		domains:  domains,
		imgHosts: imgHosts,
		retry:    cfg.Retry,
		cookies:  map[string]string{},
	}
	if cfg.Cookies != nil {
		for k, v := range cfg.Cookies {
			c.cookies[k] = v
		}
	}
	if c.retry <= 0 {
		c.retry = 5
	}
	// ensureCookies 改为惰性触发，避免启动期后台 goroutine 抢占连接。
	// 第一次 requestJSON 时会按需调用一次（见 ensureCookiesOnce）。
	return c, nil
}

// ensureCookiesOnce 用 sync.Once 保证 ensureCookies 只在首次请求时同步执行一次。
func (c *apiClient) ensureCookiesOnce(ctx context.Context) {
	c.cookieOnce.Do(func() {
		_ = c.ensureCookies(ctx)
	})
}

func (c *apiClient) ImplKey() string { return "api" }

func (c *apiClient) setCookies(cookies map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range cookies {
		c.cookies[k] = v
	}
}

func (c *apiClient) snapshotCookies() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.cookies) == 0 {
		return ""
	}
	var b bytes.Buffer
	first := true
	// 固定顺序便于复现
	for _, k := range sortedKeys(c.cookies) {
		if !first {
			b.WriteString("; ")
		}
		first = false
		fmt.Fprintf(&b, "%s=%s", k, c.cookies[k])
	}
	return b.String()
}

// ensureCookies 保证至少有一个 AVS 占位 cookie。APP API 不校验内容，但缺失会被拒。
func (c *apiClient) ensureCookies(ctx context.Context) error {
	c.mu.RLock()
	has := c.cookies["AVS"] != ""
	c.mu.RUnlock()
	if has {
		return nil
	}
	// 调 /setting 触发服务端下发 cookie。
	// 注意：这里直接走 doRequest 而非 requestJSON，避免 requestJSON 开头的
	// ensureCookiesOnce 与 sync.Once 在同一 goroutine 内重入死锁。
	ts, token, tokenParam := fixTokenTriple()
	resp, err := c.doRequest(ctx, "GET", apiPathSetting, nil, ts, token, tokenParam, false, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("APP API %s HTTP %d", apiPathSetting, resp.StatusCode)
	}
	c.mu.Lock()
	if c.cookies["AVS"] == "" {
		c.cookies["AVS"] = "0" // 占位
	}
	c.mu.Unlock()
	return nil
}

// requestJSON 发起一次 APP API 请求，自动注入 token/cookie，并解密响应 data。
//
// extraHeaders 用于附加方法专属头（如登录需要的 Content-Type）。
// 返回 (解密后的 JSON 业务字段原始字节, 解密后的整段文本, err)。
func (c *apiClient) requestJSON(ctx context.Context, method, path string, body io.Reader, ts, token, tokenParam, secret string, extraHeaders map[string]string) ([]byte, string, error) {
	// 首次请求前同步初始化 cookie（惰性，避免启动期后台 goroutine 抢占连接）。
	c.ensureCookiesOnce(ctx)
	resp, err := c.doRequest(ctx, method, path, body, ts, token, tokenParam, false, extraHeaders)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("APP API %s HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	// 解析外层 {code,data,...}
	var outer struct {
		Code int    `json:"code"`
		Data string `json:"data"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, "", fmt.Errorf("解析响应失败: %w (body=%s)", err, truncate(string(raw), 200))
	}
	if outer.Code != 200 {
		return nil, "", fmt.Errorf("APP API %s 业务错误 code=%d msg=%s", path, outer.Code, outer.Msg)
	}
	if outer.Data == "" {
		// 部分接口（如登录）data 可能为空但 code=200
		return raw, "", nil
	}
	plain, err := DecodeRespData(outer.Data, ts, secret)
	if err != nil {
		return nil, "", fmt.Errorf("解密 %s 失败: %w", path, err)
	}
	return raw, plain, nil
}

// doRequest 是底层 HTTP 调用，支持域名轮换与重试。
// extraHeaders 用于附加如 Content-Type 之类的方法专属头。
// body 若实现 io.Seeker（如 *bytes.Reader），重试前会自动 Seek 回起点。
func (c *apiClient) doRequest(ctx context.Context, method, path string, body io.Reader, ts, token, tokenParam string, isImage bool, extraHeaders map[string]string) (*http.Response, error) {
	var lastErr error
	for di := 0; di < len(c.domains); di++ {
		for attempt := 0; attempt <= c.retry; attempt++ {
			domain := c.domains[di%len(c.domains)]
			var urlStr string
			if isImage {
				urlStr = path // 图片 URL 是完整的
			} else {
				urlStr = "https://" + domain + path
			}
			// 重试前把可 Seek 的 body 重置回起点，避免第二次读到 EOF。
			if seeker, ok := body.(io.Seeker); ok {
				_, _ = seeker.Seek(0, io.SeekStart)
			}
			req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
			if err != nil {
				return nil, err
			}
			c.applyHeaders(req, token, tokenParam, isImage, domain)
			for k, v := range extraHeaders {
				req.Header.Set(k, v)
			}
			resp, err := c.http.Do(req)
			if err == nil {
				// 图片域名失败时也收集 cookie；此处仅 API 请求收集
				if !isImage {
					c.collectCookies(resp)
				}
				if resp.StatusCode < 500 {
					return resp, nil
				}
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

func (c *apiClient) applyHeaders(req *http.Request, token, tokenParam string, isImage bool, apiDomain string) {
	req.Header.Set("User-Agent", appUA)
	// 注意：不要手动设置 Accept-Encoding。Go 的 http.Client 默认会自动加上
	// "Accept-Encoding: gzip" 并在 transport 层透明解压；一旦手动设置，Go 会
	// 认为调用方要自己处理，结果 io.ReadAll 拿到的是压缩后的二进制，JSON 解析失败。
	if ck := c.snapshotCookies(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	if !isImage {
		req.Header.Set("token", token)
		req.Header.Set("tokenparam", tokenParam)
	} else {
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("X-Requested-With", "com.JMComic3.app")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
		req.Header.Set("Referer", "https://"+apiDomain+"/")
	}
}

func (c *apiClient) collectCookies(resp *http.Response) {
	for _, ck := range resp.Cookies() {
		if ck.Name == "" || ck.Value == "" {
			continue
		}
		c.mu.Lock()
		c.cookies[ck.Name] = ck.Value
		c.mu.Unlock()
	}
}

func (c *apiClient) nextImageHost() string {
	idx := atomic.AddUint32(&c.imgIdx, 1)
	return c.imgHosts[int(idx)%len(c.imgHosts)]
}

// ---- Login ----

func (c *apiClient) Login(ctx context.Context, sess domain.Session, username, password string) (domain.Session, error) {
	ts, token, tokenParam := fixTokenTriple()
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	body := form.Encode()
	_, plain, err := c.requestJSON(ctx, "POST", apiPathLogin, bytes.NewReader([]byte(body)), ts, token, tokenParam, APPTokenSecret, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if err != nil {
		return domain.Session{}, err
	}
	var data struct {
		Username string `json:"username"`
		UID      string `json:"uid"`
		Email    string `json:"email"`
		S        string `json:"s"` // 部分 API 返回 s 作为 AVS
	}
	if plain != "" {
		_ = json.Unmarshal([]byte(plain), &data)
	}
	// 登录响应会通过 Set-Cookie 下发 AVS（已在 collectCookies 收集）
	c.mu.RLock()
	cookies := map[string]string{}
	for k, v := range c.cookies {
		cookies[k] = v
	}
	c.mu.RUnlock()
	if cookies["AVS"] == "" && data.S != "" {
		cookies["AVS"] = data.S
		c.mu.Lock()
		c.cookies["AVS"] = data.S
		c.mu.Unlock()
	}
	uname := data.Username
	if uname == "" {
		uname = username
	}
	return domain.Session{
		SourceID:   SourceID,
		Username:   uname,
		Cookies:    cookies,
		ValidUntil: futureDuration(days14),
	}, nil
}

// ---- Album ----

func (c *apiClient) FetchAlbum(ctx context.Context, sess domain.Session, albumID string) (*domain.Manga, error) {
	_ = c.ensureCookies(ctx)
	ts, token, tokenParam := fixTokenTriple()
	path := fmt.Sprintf("%s?id=%s", apiPathAlbum, albumID)
	_, plain, err := c.requestJSON(ctx, "GET", path, nil, ts, token, tokenParam, APPTokenSecret, nil)
	if err != nil {
		return nil, err
	}
	return parseAlbumAPI(plain, albumID, c.coverURL(albumID))
}

// ---- Chapter ----

func (c *apiClient) FetchChapter(ctx context.Context, sess domain.Session, chapterID string) (*domain.Chapter, []domain.Page, error) {
	_ = c.ensureCookies(ctx)
	ts, token, tokenParam := fixTokenTriple()
	// 章节详情
	chPath := fmt.Sprintf("%s?id=%s", apiPathChapter, chapterID)
	_, plain, err := c.requestJSON(ctx, "GET", chPath, nil, ts, token, tokenParam, APPTokenSecret, nil)
	if err != nil {
		return nil, nil, err
	}
	ch, pageArr, scrambleID, err := parseChapterAPI(plain, chapterID)
	if err != nil {
		return nil, nil, err
	}

	// 拼接图片 URL：https://cdn-msp.{host}/media/photos/{photo_id}/{index:05d}{suffix}
	host := c.nextImageHost()
	pages := make([]domain.Page, 0, len(pageArr))
	for i, fn := range pageArr {
		name, suffix := splitFileName(fn)
		idx := i + 1
		imgURL := fmt.Sprintf("https://%s/media/photos/%s/%05d%s", host, chapterID, idx, suffix)
		pages = append(pages, domain.Page{
			Index:      idx,
			URL:        imgURL,
			FileName:   name,
			ScrambleID: scrambleID,
		})
	}
	return ch, pages, nil
}

// ---- Favorites ----

func (c *apiClient) FetchFavorites(ctx context.Context, sess domain.Session, folderID string, page int) (*domain.FavoritePage, error) {
	if sess.Cookies["AVS"] == "" && c.snapshotCookies() == "" {
		return nil, errors.New("收藏夹需要先登录")
	}
	_ = c.ensureCookies(ctx)
	// 注入登录 cookie
	if sess.Cookies != nil {
		c.setCookies(sess.Cookies)
	}
	ts, token, tokenParam := fixTokenTriple()
	folder := folderID
	if folder == "" {
		folder = "0"
	}
	path := fmt.Sprintf("%s?page=%d&folder_id=%s&o=mr", apiPathFavorite, page, folder)
	_, plain, err := c.requestJSON(ctx, "GET", path, nil, ts, token, tokenParam, APPTokenSecret, nil)
	if err != nil {
		return nil, err
	}
	fp, err := parseFavoritesAPI(plain, folderID, page)
	if err != nil {
		return nil, err
	}
	// 收藏列表接口不下发封面图（image 字段为空），统一用 coverURL 模式补全。
	for i := range fp.Items {
		if fp.Items[i].CoverURL == "" {
			fp.Items[i].CoverURL = c.coverURL(fp.Items[i].MangaID)
		}
	}
	return fp, nil
}

// ---- Image ----

func (c *apiClient) FetchImage(ctx context.Context, sess domain.Session, pg domain.Page) (domain.ImageData, error) {
	// 图片请求不需要 token，但需要占位 cookie 与正确的 Referer/UA
	ts := newTimestamp()
	resp, err := c.doRequest(ctx, "GET", pg.URL, nil, ts, "", "", true, nil)
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

// ---- 辅助 ----

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt*attempt*50) * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单冒泡（key 通常很少）
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func (c *apiClient) coverURL(albumID string) string {
	host := c.nextImageHost()
	return fmt.Sprintf("https://%s/media/albums/%s.jpg", host, albumID)
}

// splitFileName 把 "00001.webp" 拆为 ("00001", ".webp")。
func splitFileName(fn string) (name, suffix string) {
	for i := len(fn) - 1; i >= 0; i-- {
		if fn[i] == '.' {
			return fn[:i], fn[i:]
		}
	}
	return fn, ".jpg"
}

// 占位：保留 strconv 引用以便后续字段处理
var _ = strconv.Itoa
