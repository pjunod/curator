package ports

import (
	"context"
	"errors"
)

// ErrNotifyPermanent marks a delivery failure that retrying cannot fix: a
// path outside every library root, a key without the scope, a revoked
// credential. Wrap it and the delivery queue stops immediately instead of
// spending its whole backoff schedule postponing the moment somebody reads
// the reason.
var ErrNotifyPermanent = errors.New("permanent delivery failure")

// NotifierConfig is a stored notification target (Phase 3). Settings is
// type-specific: webhook/discord need {"url"}, plex needs {"url","token"},
// jellyfin and plurx need {"url","apiKey"}.
type NotifierConfig struct {
	ID       int64
	Type     string // webhook | discord | plex | jellyfin | plurx
	Name     string
	Settings map[string]string
	OnGrab   bool
	OnImport bool
	OnFailed bool
	OnHealth bool
	Enabled  bool
}

// Notification is one event rendered for the outside world. The JSON shape
// is the webhook wire format — lowercase, stable.
type Notification struct {
	Event string `json:"event"` // grab | import | failed | health | test
	Title string `json:"title"`
	Body  string `json:"body"`
	// Fields carries structured extras (release, quality, item title…).
	Fields map[string]string `json:"fields,omitempty"`
	// Import is the machine-readable half of an import event: what landed,
	// where, and what it is. Present only on `import`.
	//
	// Chat notifiers have no use for it and ignore it; the webhook passes it
	// through, which is the point of a webhook. A media server that can be
	// told "index exactly this path, it is tmdb 949" needs all of it — the
	// difference between one folder indexed correctly and a whole library
	// swept and then matched by guessing at a filename.
	Import *ImportInfo `json:"import,omitempty"`
	// IdempotencyKey is "<sub-delivery key>/<attempt>" (contract section 4.2),
	// set by the delivery worker on each attempt and never stored in the
	// payload: Cinema returns the same request for a repeated key, so a lost
	// response does not create a second scan.
	IdempotencyKey string `json:"-"`
}

// ImportInfo is the structured detail behind an import notification.
type ImportInfo struct {
	MediaItemID int64 `json:"mediaItemId,omitempty"`
	DownloadID  int64 `json:"downloadId,omitempty"`
	// Paths are every placed file; Dirs are their unique parent
	// directories — what a media server is actually asked to index. Both
	// are absolute as MONARR sees them: a consumer on another host may
	// need a path mapping, the same caveat download clients carry.
	Paths []string `json:"paths,omitempty"`
	Dirs  []string `json:"dirs,omitempty"`
	// Kind is movie | series | book. The plurx notifier uses it to keep
	// movie ids, series ids, and explicit Books work/edition facts on their
	// honest request shapes.
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title,omitempty"`
	// Book carries Curator's explicit edition facts. It is absent for every
	// non-book import and for a legacy/incomplete book event. WorkID, never a
	// title/author comparison, is what allows Cinema to relate editions.
	Book *BookImportInfo `json:"book,omitempty"`
	// TmdbID and ImdbID identify the item: for a series, the SHOW.
	TmdbID int64  `json:"tmdbId,omitempty"`
	ImdbID string `json:"imdbId,omitempty"`
	// Transfer names this transfer end to end, across applications.
	Transfer string `json:"transfer,omitempty"`
	// PublicationID is the publication this import came from (0 for an
	// import that did not come from one).
	PublicationID int64 `json:"publicationId,omitempty"`
	// ExpectedFiles are the media files the import placed, keyed by their
	// directory (one of Dirs), with the size and nanosecond mtime they had
	// once placed. Cinema re-probes exactly these and reports each back; a
	// sub-delivery is visible only when every one comes back placed with
	// matching bytes and mtime_ns (contract section 4.2).
	ExpectedFiles map[string][]ExpectedFile `json:"expectedFiles,omitempty"`
}

// ExpectedFile is one media file an import placed, as it stood once placed.
type ExpectedFile struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	MtimeNs int64  `json:"mtime_ns"`
}

// BookImportInfo is the additive Curator → Cinema book handoff contract.
// Medium is ebook | audiobook. CoverURL is omitted unless it is an HTTPS
// Open Library cover URL; Cinema validates the same boundary before fetch.
type BookImportInfo struct {
	Title     string `json:"title,omitempty"`
	Author    string `json:"author,omitempty"`
	Medium    string `json:"medium"`
	WorkID    string `json:"work_id"`
	EditionID string `json:"edition_id"`
	CoverURL  string `json:"cover_url,omitempty"`
}

// Notifier delivers notifications. Library-refresh notifiers (Plex,
// Jellyfin) ignore the payload and poke the media server instead.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
	Test(ctx context.Context) error
}

// DeliveryReporter is an optional capability: a notifier whose far side
// answered with something worth keeping.
//
// Optional because most have nothing to say — a webhook returning 204 tells
// you it was accepted and no more. plurx answers a scan with what it made of
// the files, and that belongs in the transfer's trace: it is the only place
// the chain "grabbed → downloaded → imported → indexed as item 1201" is ever
// joined up. Asserted for after Send, in the same shape as the download
// clients' TaggedAdder and Subscriber.
type DeliveryReporter interface {
	// Delivery summarizes the last Send in one line, or "" if there is
	// nothing to add.
	Delivery() string
}

// QueuedNotifier reports whether a notifier's deliveries go through the
// durable outbox rather than being attempted inline: the ones whose message
// IS the work (a media server told what to index). A chat notifier's late
// retry is worse than none.
func QueuedNotifier(cfg NotifierConfig) bool { return cfg.Type == "plurx" }

// ScanSender is the capability of a notifier that is told one directory at a
// time and answers with what it made of it (Cinema, contract section 4.2).
// The delivery worker drives one sub-delivery per call.
type ScanSender interface {
	// SendScan posts the notification's single directory with
	// n.IdempotencyKey, the directory's expected files and a short wait. An
	// error is a *NotifyError saying whether trying again can help and
	// whether the next try needs a fresh attempt.
	SendScan(ctx context.Context, n Notification) (NotifyResult, error)
}

// ResultReader polls a request the far side queued (Cinema's
// GET /api/v1/scan/requests/{id}).
type ResultReader interface {
	ReadResult(ctx context.Context, remoteRequestID string) (NotifyResult, error)
}

// PathTester is the capability of a notifier that can check, at setup, that
// it sees the library roots Curator places files under (contract section
// 4.3). Note is a pass that still has something to say ("path checking needs
// a newer cinema").
type PathTester interface {
	TestPaths(ctx context.Context, roots []string) (note string, err error)
}

// NotifyResult is what the far side said about one request.
type NotifyResult struct {
	State     string // queued | running | done | failed
	RequestID string
	WaitingOn string
	// Files is the per-file report; FilesReported is false when the far
	// side sent no per-file report at all (an older Cinema), which is a
	// different answer from an empty one.
	Files         []PlacedReport
	FilesReported bool
	ItemIDs       []int64
	Err           string
	ErrCode       string
}

// PlacedReport is Cinema's word on one expected file.
type PlacedReport struct {
	Path    string `json:"path"`
	State   string `json:"state"` // placed | skipped | missing
	Bytes   int64  `json:"bytes,omitempty"`
	MtimeNs int64  `json:"mtime_ns,omitempty"`
	ItemID  int64  `json:"item_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// NotifyRetry says what a failed request means for the next one.
type NotifyRetry int

const (
	// RetrySameAttempt: the outcome is ambiguous or the request was refused
	// before it was recorded (a timeout, a connection error, 429, 5xx); the
	// next try reuses the same key, so a request that did land is replayed,
	// not duplicated.
	RetrySameAttempt NotifyRetry = iota
	// RetryNewAttempt: a confirmed retryable result (503 path_unreadable, a
	// failed or vanished request); the next try needs a fresh scan.
	RetryNewAttempt
	// RetryNever: retrying cannot help (401/403, 400, 422 path_outside_roots).
	RetryNever
	// RetryUnsupported: the far side does not report results (an older
	// Cinema refused the status read); the request itself was accepted.
	RetryUnsupported
)

// NotifyError is a classified delivery failure.
type NotifyError struct {
	Retry  NotifyRetry
	Status int    // HTTP status, 0 when there was no response
	Code   string // the far side's error code, when it sent one
	Msg    string
}

func (e *NotifyError) Error() string { return e.Msg }

// Is makes errors.Is(err, ErrNotifyPermanent) true for a RetryNever error.
func (e *NotifyError) Is(target error) bool {
	return target == ErrNotifyPermanent && e.Retry == RetryNever
}
