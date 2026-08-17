// Package hook defines the canonical event topics plugins can react to.
//
// Hooks are delivered through plugin.EventBus; a plugin simply calls
// ctx.Bus.On(hook.TopicMangaAfter, ...). Defining the topics in one place keeps
// producers and consumers in sync and documents the lifecycle.
package hook

const (
	// Download lifecycle (fired by the download engine).
	DownloadQueued      = "download.queued"
	DownloadStart       = "download.start"
	DownloadChapterDone = "download.chapter.done"
	DownloadImageBefore = "download.image.before"
	DownloadImageAfter  = "download.image.after"
	DownloadComplete    = "download.complete"
	DownloadFailed      = "download.failed"
	DownloadProgress    = "download.progress"

	// Library lifecycle.
	MangaAdded    = "library.manga.added"
	MangaUpdated  = "library.manga.updated"
	MangaRemoved  = "library.manga.removed"

	// Sync lifecycle.
	SyncTick      = "sync.tick"
	SyncNewManga  = "sync.new.manga"
	SyncNewChapter = "sync.new.chapter"

	// Sink lifecycle.
	SinkBeforeUpload = "sink.before.upload"
	SinkAfterUpload  = "sink.after.upload"

	// Host lifecycle.
	HostDispose = "host.dispose"
)

// Payload keys commonly used across topics.
const (
	KeySourceID   = "source_id"
	KeyMangaID    = "manga_id"
	KeyChapterID  = "chapter_id"
	KeyPage       = "page"
	KeyTotal      = "total"
	KeyDone       = "done"
	KeyPath       = "path"
	KeyErr        = "error"
	KeyTaskID     = "task_id"
)
