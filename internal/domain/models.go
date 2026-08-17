// Package domain defines the source-agnostic vocabulary used across the app.
//
// Every source plugin (JMComic, future ones) maps its own native model onto
// these structs. This decouples the library/download/UI code from any single
// provider.
package domain

import "time"

// Manga is a whole work (JMComic "album"). It may contain many Chapters.
type Manga struct {
	ID          string    `json:"id"`            // source-scoped id
	SourceID    string    `json:"source_id"`     // which plugin produced it
	Title       string    `json:"title"`
	Author      string    `json:"author"`
	Description string    `json:"description"`
	CoverURL    string    `json:"cover_url"`
	Tags        []string  `json:"tags"`
	Works       []string  `json:"works"`
	Actors      []string  `json:"actors"`
	PageCount   int       `json:"page_count"` // total pages across chapters
	PubDate     string    `json:"pub_date"`
	UpdateDate  string    `json:"update_date"`
	Likes       string    `json:"likes"`
	Views       string    `json:"views"`
	Chapters    []Chapter `json:"chapters"`
	ScrambleID  string    `json:"scramble_id,omitempty"` // JM-specific, kept for decode

	// Library bookkeeping (filled by the library service, not the source).
	AddedAt     time.Time `json:"added_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Downloaded  bool      `json:"downloaded"`
	LocalPath   string    `json:"local_path,omitempty"`
}

// Chapter is one installment of a manga (JMComic "photo").
type Chapter struct {
	ID         string `json:"id"`
	MangaID    string `json:"manga_id"`
	Title      string `json:"title"`
	Index      int    `json:"index"`     // 1-based ordering within the manga
	PageCount  int    `json:"page_count"`
	ScrambleID string `json:"scramble_id,omitempty"`
}

// ReadingProgress records where the user left off in a manga.
// Stored server-side so reading continues across devices.
// Single-chapter works only ever record ChapterID (their one chapter).
type ReadingProgress struct {
	SourceID   string    `json:"source_id"`
	MangaID    string    `json:"manga_id"`
	ChapterID  string    `json:"chapter_id"`
	Page       int       `json:"page"`        // 1-based page within the chapter
	TotalPages int       `json:"total_pages"` // chapter's page count, for progress display
	UpdatedAt  time.Time `json:"updated_at"`
}

// Page is a single image within a chapter. Index is 1-based.
type Page struct {
	Index      int    `json:"index"`
	URL        string `json:"url"`         // remote url (for downloading)
	FileName   string `json:"file_name"`   // local filename within chapter dir
	ScrambleID string `json:"scramble_id"` // used by the decode step
	Width      int    `json:"width,omitempty"`
}

// Favorite is an entry in a remote favorites/collection list.
type Favorite struct {
	MangaID  string   `json:"manga_id"`
	SourceID string   `json:"source_id"`
	Title    string   `json:"title"`
	CoverURL string   `json:"cover_url"`
	Author   string   `json:"author"`
	Tags     []string `json:"tags"`
	Folder   string   `json:"folder,omitempty"`
}

// FavoritePage is a page of favorites from a source.
type FavoritePage struct {
	Items    []Favorite `json:"items"`
	Page     int        `json:"page"`
	Pages    int        `json:"pages"`
	Total    int        `json:"total"`
	FolderID string     `json:"folder_id,omitempty"`
	Folders  []Folder   `json:"folders,omitempty"`
}

// Folder is a remote favorites folder.
type Folder struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Credentials carry per-source login material.
type Credentials struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Cookies  map[string]string `json:"cookies,omitempty"`
	Token    string `json:"token,omitempty"`
	Raw      map[string]string `json:"raw,omitempty"`
}

// Session is an authenticated handle returned by Source.Login.
type Session struct {
	SourceID   string            `json:"source_id"`
	Username   string            `json:"username"`
	Cookies    map[string]string `json:"cookies"`
	ValidUntil time.Time         `json:"valid_until"`
}

// ImageData is a fetched image plus the metadata needed to decode it.
type ImageData struct {
	Reader      []byte  // raw image bytes (still scrambled if ScrambleID implies so)
	URL         string
	Suffix      string  // e.g. ".webp", ".jpg"
	ScrambleID  string
	ContentType string
}
