package jmcomic

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"tsukimi/internal/domain"
)

// ---- 公共正则（来自参考项目 jmcomic.jm_toolkit，用于 html client） ----

var (
	htmlB64ContentRE = regexp.MustCompile(`const html = base64DecodeUtf8\("(.*?)"\)`)

	// album
	albumIDRE       = regexp.MustCompile(`<span class="number">.*?[：:]JM(\d+)</span>`)
	albumScrambleRE = regexp.MustCompile(`var scramble_id = (\d+);`)
	albumNameRE     = regexp.MustCompile(`id="book-name"[^>]*?>([\s\S]*?)<`)
	albumDescRE     = regexp.MustCompile(`[叙|敘]述[：:]([\s\S]*?)</h2>`)
	albumEpisodeRE  = regexp.MustCompile(`data-album="(\d+)"[^>]*>[\s\S]*?第(\d+)[话話]([\s\S]*?)<`)
	albumPageCntRE  = regexp.MustCompile(`<span class="pagecount">.*?[：:](\d+)</span>`)
	albumPubRE      = regexp.MustCompile(`>上架日期[ ]*:[ ]*(.*?)</span>`)
	albumUpdateRE   = regexp.MustCompile(`>更新日期[ ]*:[ ]*(.*?)</span>`)
	albumLikesRE    = regexp.MustCompile(`<span id="albim_likes_\d+">(.*?)</span>`)
	albumViewsRE    = regexp.MustCompile(`<span>(.*?)</span>\s*\n?\s*<span>(次觀看|观看次数|次观看次数)</span>`)
	tagAInnerRE     = regexp.MustCompile(`<a[^>]*?>\s*(\S*?)\s*</a>`)

	worksSpanRE  = regexp.MustCompile(`<span itemprop="author" data-type="works">([\s\S]*?)</span>`)
	actorSpanRE  = regexp.MustCompile(`<span itemprop="author" data-type="actor">([\s\S]*?)</span>`)
	tagSpanRE    = regexp.MustCompile(`<span itemprop="genre" data-type="tags">([\s\S]*?)</span>`)
	authorSpanRE = regexp.MustCompile(`<span itemprop="author" data-type="author">([\s\S]*?)</span>`)

	// photo
	photoScrambleRE = regexp.MustCompile(`var scramble_id = (\d+);`)
	photoSeriesRE   = regexp.MustCompile(`var series_id = (\d+);`)
	photoSortRE     = regexp.MustCompile(`var sort = (\d+);`)
	photoPageArrRE  = regexp.MustCompile(`var page_arr = (\[.*?\]);`)
	photoDomainRE   = regexp.MustCompile(`src="https?://(.*?)/media/albums/blank`)

	// favorites
	favCardRE    = regexp.MustCompile(`<a[^>]+href="/album/(\d+)/?"[^>]*>([\s\S]*?)</a>`)
	favTitleRE   = regexp.MustCompile(`title="([^"]*)"`)
	favImgRE     = regexp.MustCompile(`src="(https?://[^"]*?/media/albums/[^"]*?)"`)
)

// ---- 工具函数 ----

func matchGroup(re *regexp.Regexp, s string, n int) string {
	m := re.FindStringSubmatch(s)
	if m == nil || n >= len(m) {
		return ""
	}
	return m[n]
}

func mustAtoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return "", err
		}
	}
	return string(b), nil
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}

func parseJMID(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	if isAllDigit(text) {
		return text
	}
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "jm") {
		return text[2:]
	}
	if m := regexp.MustCompile(`(?:album|photo|albums|photos)/(\d+)`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile(`id=(\d+)`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return text
}

func isAllDigit(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func guessSuffix(u string) string {
	idx := strings.LastIndex(u, ".")
	if idx < 0 {
		return ".jpg"
	}
	tail := strings.ToLower(u[idx:])
	if q := strings.Index(tail, "?"); q >= 0 {
		tail = tail[:q]
	}
	switch tail {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return tail
	}
	return ".jpg"
}

func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(decodeEntities(s))
}

func decodeEntities(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", `'`, "&nbsp;", " ",
	)
	return r.Replace(s)
}

func extractTags(spanHTML string) []string {
	if spanHTML == "" {
		return nil
	}
	raw := tagAInnerRE.FindAllStringSubmatch(spanHTML, -1)
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, m := range raw {
		t := cleanText(m[1])
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ---- album HTML 解析 ----

type parseError struct{ Field, Source string }

func (e *parseError) Error() string {
	return "jmcomic: 解析失败 field=" + e.Field + " source=" + e.Source
}

type epRecord struct{ photoID, idxStr, title string }

func sortEps(eps []epRecord) {
	for i := 1; i < len(eps); i++ {
		j := i
		for j > 0 && mustAtoi(eps[j].idxStr) < mustAtoi(eps[j-1].idxStr) {
			eps[j], eps[j-1] = eps[j-1], eps[j]
			j--
		}
	}
}

func parseAlbumHTML(html, coverURL string) (*domain.Manga, error) {
	albumID := matchGroup(albumIDRE, html, 1)
	if albumID == "" {
		return nil, &parseError{Field: "album_id", Source: "album"}
	}
	scrambleID := matchGroup(albumScrambleRE, html, 1)
	name := cleanText(matchGroup(albumNameRE, html, 1))
	desc := cleanText(matchGroup(albumDescRE, html, 1))
	pageCount := mustAtoi(matchGroup(albumPageCntRE, html, 1))
	pubDate := cleanText(matchGroup(albumPubRE, html, 1))
	updateDate := cleanText(matchGroup(albumUpdateRE, html, 1))
	likes := cleanText(matchGroup(albumLikesRE, html, 1))
	views := matchGroup(albumViewsRE, html, 1)

	works := extractTags(matchGroup(worksSpanRE, html, 1))
	actors := extractTags(matchGroup(actorSpanRE, html, 1))
	tags := extractTags(matchGroup(tagSpanRE, html, 1))
	authors := extractTags(matchGroup(authorSpanRE, html, 1))

	var chapters []domain.Chapter
	episodeMatches := albumEpisodeRE.FindAllStringSubmatch(html, -1)
	if len(episodeMatches) == 0 {
		chapters = []domain.Chapter{{
			ID: albumID, MangaID: albumID, Title: name, Index: 1,
			PageCount: pageCount, ScrambleID: scrambleID,
		}}
	} else {
		eps := make([]epRecord, 0, len(episodeMatches))
		seen := map[string]bool{}
		for _, m := range episodeMatches {
			id, idx, title := m[1], m[2], cleanText(m[3])
			if seen[id+idx] {
				continue
			}
			seen[id+idx] = true
			eps = append(eps, epRecord{id, idx, title})
		}
		sortEps(eps)
		for i, e := range eps {
			chapters = append(chapters, domain.Chapter{
				ID: e.photoID, MangaID: albumID,
				Title:     fmtEpisodeTitle(i+1, e.title),
				Index:     i + 1,
				ScrambleID: scrambleID,
			})
		}
	}

	author := ""
	if len(authors) > 0 {
		author = authors[0]
	}
	return &domain.Manga{
		ID: albumID, SourceID: SourceID, Title: name, Author: author,
		Description: desc, CoverURL: coverURL, Tags: tags, Works: works,
		Actors: actors, PageCount: pageCount, PubDate: pubDate, UpdateDate: updateDate,
		Likes: likes, Views: cleanText(views), Chapters: chapters, ScrambleID: scrambleID,
	}, nil
}

// ---- photo HTML 解析 ----

// parsePhotoHTML 解析章节页 HTML。
// imgDomain 用于拼接图片 URL（HTML 端从页面里取）。
func parsePhotoHTML(html, chapterID, imgDomain string) (*domain.Chapter, []domain.Page, error) {
	scrambleID := matchGroup(photoScrambleRE, html, 1)
	seriesID := matchGroup(photoSeriesRE, html, 1)
	sort := matchGroup(photoSortRE, html, 1)
	pageArrJSON := matchGroup(photoPageArrRE, html, 1)
	if imgDomain == "" {
		imgDomain = matchGroup(photoDomainRE, html, 1)
	}
	if imgDomain == "" {
		imgDomain = "cdn-msp2.18comic.org"
	}
	var fileNames []string
	if pageArrJSON != "" {
		_ = jsonUnmarshalStringSlice(pageArrJSON, &fileNames)
	}
	if len(fileNames) == 0 {
		return nil, nil, &parseError{Field: "page_arr", Source: "photo"}
	}
	mangaID := chapterID
	if seriesID != "0" && seriesID != "" {
		mangaID = seriesID
	}
	index := mustAtoi(sort)
	if index == 0 {
		index = 1
	}
	pages := make([]domain.Page, 0, len(fileNames))
	for i, fn := range fileNames {
		idx := i + 1
		name, suffix := splitFileName(fn)
		imgURL := fmt.Sprintf("https://%s/media/photos/%s/%s", imgDomain, chapterID, fn)
		pages = append(pages, domain.Page{
			Index: idx, URL: imgURL, FileName: name, ScrambleID: scrambleID,
		})
		_ = suffix
	}
	ch := &domain.Chapter{
		ID: chapterID, MangaID: mangaID, Index: index,
		ScrambleID: scrambleID, PageCount: len(pages),
	}
	return ch, pages, nil
}

// ---- favorites HTML 解析 ----

func parseFavoritesHTML(html, folderID string, page int) (*domain.FavoritePage, error) {
	matches := favCardRE.FindAllStringSubmatch(html, -1)
	items := make([]domain.Favorite, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		block := m[2]
		title := cleanText(matchGroup(favTitleRE, block, 1))
		cover := matchGroup(favImgRE, block, 1)
		items = append(items, domain.Favorite{
			MangaID: id, SourceID: SourceID, Title: title, CoverURL: cover, Folder: folderID,
		})
	}
	pages := []string{}
	for _, p := range paginationPages(html) {
		pages = append(pages, p)
	}
	maxPage := page
	for _, p := range pages {
		if v := mustAtoi(p); v > maxPage {
			maxPage = v
		}
	}
	return &domain.FavoritePage{
		Items: items, Page: page, Pages: maxPage, Total: len(items), FolderID: folderID,
	}, nil
}

func paginationPages(html string) []string {
	re := regexp.MustCompile(`href="[^"]*page=(\d+)[^"]*"`)
	out := re.FindAllStringSubmatch(html, -1)
	res := make([]string, 0, len(out))
	for _, m := range out {
		res = append(res, m[1])
	}
	return res
}

// ---- 其它辅助 ----

func fmtEpisodeTitle(idx int, title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "第" + strconv.Itoa(idx) + "话"
	}
	return title
}

// jsonUnmarshalStringSlice 宽松解析 JSON 字符串数组。
func jsonUnmarshalStringSlice(s string, out *[]string) error {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return fmt.Errorf("not a json array")
	}
	// 简易正则提取字符串元素
	re := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	matches := re.FindAllStringSubmatch(s, -1)
	res := make([]string, 0, len(matches))
	for _, m := range matches {
		res = append(res, unescapeJSON(m[1]))
	}
	*out = res
	return nil
}

func unescapeJSON(s string) string {
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\/`, `/`)
	return r.Replace(s)
}
