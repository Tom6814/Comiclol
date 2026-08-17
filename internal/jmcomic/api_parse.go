package jmcomic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"tsukimi/internal/domain"
)

// APP API 解密后的 JSON 解析。
//
// 注意：字段结构主要依据 jmcomic.JmApiAdaptTool 与接口惯例。部分字段（尤其是
// 收藏夹列表的嵌套）在没有真实抓包样本的情况下属于合理推断，解析采用宽松策略
// （缺失字段不报错），便于在真实环境下逐步校正。

// ---- 通用取值辅助 ----

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(t)
		return i
	case int:
		return t
	case int64:
		return int(t)
	}
	return 0
}

func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s := strings.TrimSpace(asString(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// asMapSlice 把 any[] 当作 map[string]any 列表返回。
func asMapSlice(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func mapGet(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok {
		return v
	}
	return nil
}

// ---- album 解析 ----

func parseAlbumAPI(plain, albumID, coverURL string) (*domain.Manga, error) {
	if plain == "" {
		return nil, fmt.Errorf("album 响应为空")
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(plain), &root); err != nil {
		return nil, fmt.Errorf("album JSON 解析失败: %w", err)
	}
	// 部分接口把业务字段包在 "data" 里，部分直接平铺
	if inner, ok := root["data"].(map[string]any); ok {
		root = inner
	}

	id := asString(mapGet(root, "id"))
	if id == "" {
		id = albumID
	}
	name := asString(mapGet(root, "name"))
	authors := asStringSlice(mapGet(root, "author"))
	desc := asString(mapGet(root, "description"))
	tags := asStringSlice(mapGet(root, "tags"))
	works := asStringSlice(mapGet(root, "works"))
	actors := asStringSlice(mapGet(root, "actors"))
	likes := asString(mapGet(root, "likes"))
	views := asString(mapGet(root, "total_views"))
	scrambleID := asString(mapGet(root, "scramble_id"))
	pubDate := asString(mapGet(root, "pub_date"))
	if pubDate == "" {
		pubDate = asString(mapGet(root, "addtime"))
	}
	updateDate := asString(mapGet(root, "update_date"))
	if updateDate == "" {
		updateDate = asString(mapGet(root, "uptime"))
	}

	// 章节列表：series 数组 [{id, sort, name}]
	series := asMapSlice(mapGet(root, "series"))
	chapters := buildChaptersFromSeries(series, id, scrambleID, name)

	pageCount := asInt(mapGet(root, "page_count"))
	if pageCount == 0 {
		// 无明确字段时用章节图片数之和近似
		for _, c := range chapters {
			pageCount += c.PageCount
		}
	}

	author := ""
	if len(authors) > 0 {
		author = authors[0]
	}

	return &domain.Manga{
		ID:          id,
		SourceID:    SourceID,
		Title:       strings.TrimSpace(name),
		Author:      author,
		Description: desc,
		CoverURL:    coverURL,
		Tags:        tags,
		Works:       works,
		Actors:      actors,
		PageCount:   pageCount,
		PubDate:     pubDate,
		UpdateDate:  updateDate,
		Likes:       likes,
		Views:       views,
		Chapters:    chapters,
		ScrambleID:  scrambleID,
	}, nil
}

// buildChaptersFromSeries 把 series 数组转成 domain.Chapter；为空则视为单章本子。
func buildChaptersFromSeries(series []map[string]any, albumID, scrambleID, name string) []domain.Chapter {
	if len(series) == 0 {
		return []domain.Chapter{{
			ID:         albumID,
			MangaID:    albumID,
			Title:      name,
			Index:      1,
			ScrambleID: scrambleID,
		}}
	}
	type item struct {
		sort int
		m    map[string]any
	}
	items := make([]item, 0, len(series))
	for _, s := range series {
		items = append(items, item{sort: asInt(mapGet(s, "sort")), m: s})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].sort < items[j].sort })
	chapters := make([]domain.Chapter, 0, len(items))
	for i, it := range items {
		cid := asString(mapGet(it.m, "id"))
		if cid == "" {
			cid = albumID
		}
		title := asString(mapGet(it.m, "name"))
		if title == "" {
			title = fmt.Sprintf("第%d话", i+1)
		}
		chapters = append(chapters, domain.Chapter{
			ID:         cid,
			MangaID:    albumID,
			Title:      title,
			Index:      i + 1,
			ScrambleID: scrambleID,
		})
	}
	return chapters
}

// ---- chapter（photo）解析 ----

func parseChapterAPI(plain, chapterID string) (*domain.Chapter, []string, string, error) {
	if plain == "" {
		return nil, nil, "", fmt.Errorf("chapter 响应为空")
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(plain), &root); err != nil {
		return nil, nil, "", fmt.Errorf("chapter JSON 解析失败: %w", err)
	}
	if inner, ok := root["data"].(map[string]any); ok {
		root = inner
	}
	id := asString(mapGet(root, "id"))
	if id == "" {
		id = chapterID
	}
	name := asString(mapGet(root, "name"))
	scrambleID := asString(mapGet(root, "scramble_id"))
	mangaID := asString(mapGet(root, "series_id"))
	if mangaID == "" {
		mangaID = asString(mapGet(root, "album_id"))
	}
	if mangaID == "" {
		mangaID = id
	}
	// images 字段可能是字符串数组 ["00001.webp",...] 或对象数组
	pageArr := asStringSlice(mapGet(root, "images"))
	if len(pageArr) == 0 {
		// 尝试 images 为对象数组的情况
		if imgs := asMapSlice(mapGet(root, "images")); len(imgs) > 0 {
			for _, im := range imgs {
				if url := asString(mapGet(im, "url")); url != "" {
					pageArr = append(pageArr, url)
				} else if fn := asString(mapGet(im, "name")); fn != "" {
					pageArr = append(pageArr, fn)
				}
			}
		}
	}
	ch := &domain.Chapter{
		ID:         id,
		MangaID:    mangaID,
		Title:      name,
		ScrambleID: scrambleID,
		PageCount:  len(pageArr),
	}
	// 章节序号从 series 中查找；这里默认置 1，由 manga 详情统一排序后修正
	ch.Index = 1
	return ch, pageArr, scrambleID, nil
}

// ---- 收藏夹解析 ----

// parseFavoritesAPI 解析收藏夹列表。结构为推断：
//
//	{ "list": [ {id,name,author,tags,...}, ... ], "total": N, "folder_list": [...] }
//
// 兼容字段别名：albums / data / list；count / total。
func parseFavoritesAPI(plain, folderID string, page int) (*domain.FavoritePage, error) {
	if plain == "" {
		return &domain.FavoritePage{Page: page, FolderID: folderID, Items: []domain.Favorite{}}, nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(plain), &root); err != nil {
		return nil, fmt.Errorf("favorite JSON 解析失败: %w", err)
	}
	if inner, ok := root["data"].(map[string]any); ok {
		root = inner
	}
	// 列表字段兼容
	var rawList []map[string]any
	for _, key := range []string{"list", "albums", "items", "content"} {
		if v := asMapSlice(mapGet(root, key)); len(v) > 0 {
			rawList = v
			break
		}
	}
	total := asInt(mapGet(root, "total"))
	if total == 0 {
		total = asInt(mapGet(root, "count"))
	}
	if total == 0 {
		total = len(rawList)
	}

	items := make([]domain.Favorite, 0, len(rawList))
	for _, m := range rawList {
		id := asString(mapGet(m, "id"))
		if id == "" {
			id = asString(mapGet(m, "album_id"))
		}
		if id == "" {
			continue
		}
		title := asString(mapGet(m, "name"))
		if title == "" {
			title = asString(mapGet(m, "title"))
		}
		authors := asStringSlice(mapGet(m, "author"))
		author := ""
		if len(authors) > 0 {
			author = authors[0]
		}
		items = append(items, domain.Favorite{
			MangaID:  id,
			SourceID: SourceID,
			Title:    strings.TrimSpace(title),
			CoverURL: asString(mapGet(m, "cover_url")),
			Author:   author,
			Tags:     asStringSlice(mapGet(m, "tags")),
			Folder:   folderID,
		})
	}

	// 收藏夹分类
	var folders []domain.Folder
	for _, f := range asMapSlice(mapGet(root, "folder_list")) {
		folders = append(folders, domain.Folder{
			ID:    asString(mapGet(f, "id")),
			Name:  asString(mapGet(f, "name")),
			Count: asInt(mapGet(f, "count")),
		})
	}

	pages := 1
	if total > 0 && len(items) > 0 {
		// 收藏夹分页大小通常 20
		pageSize := 20
		if len(items) > 0 && page == 1 && total > len(items) {
			pageSize = len(items)
		}
		pages = (total + pageSize - 1) / pageSize
		if pages < 1 {
			pages = 1
		}
	}

	return &domain.FavoritePage{
		Items:    items,
		Page:     page,
		Pages:    pages,
		Total:    total,
		FolderID: folderID,
		Folders:  folders,
	}, nil
}
