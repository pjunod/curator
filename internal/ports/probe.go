package ports

import (
	"context"
	"fmt"
	"github.com/pjunod/monarr/internal/domain/hdr"
)

// ReleaseProber is an optional download-client capability (ADR 0026 §6): read
// the first bytes of a release's main media file without downloading the
// release. The client returns bytes, never an opinion about them — Curator
// measures them with the same prober it runs on imported files, so a
// pre-flight measurement and a post-import one cannot disagree.
//
// Asserted for, like TaggedAdder: a client without it is simply never asked.
type ReleaseProber interface {
	// SupportsReleaseProbe negotiates the capability with the client; a
	// client that answers false (an older Runner) is not asked to probe.
	SupportsReleaseProbe(ctx context.Context) (bool, error)
	// ProbeRelease fetches the release the URL names and returns up to
	// maxBytes of its main file's head. A release whose head cannot be read
	// returns a *ProbeRefusal naming why.
	ProbeRelease(ctx context.Context, downloadURL string, maxBytes int64) (ReleaseHead, error)
}

// ReleaseHead is the decoded head of a release's main media file.
type ReleaseHead struct {
	// File is the inner file's name and FileSize its full length, which the
	// prober needs to judge the container (a Matroska Segment's declared
	// size, an MP4 whose moov sits at the end).
	File     string
	FileSize int64
	// Path is how the bytes were reached: "yenc", then "rar-store" when the
	// file sat in an uncompressed RAR volume.
	Path []string
	// Articles is how many NNTP articles were read; Missing how many of
	// those the servers no longer had (the head is then cut short there).
	Articles int
	Missing  int
	Bytes    []byte
}

// Probe refusal reasons, as Runner names them.
const (
	ProbeEncrypted       = "encrypted"        // the archive is password-protected
	ProbeCompressed      = "compressed"       // the archive is not store-mode
	ProbeMissingArticles = "missing_articles" // the head's articles are gone
	ProbeNoMedia         = "no_media"         // no media file in the release
	ProbeUnreadable      = "unreadable"       // anything else Runner could not decode
)

// ProbeRefusal is a client's answer that the head cannot be read, with why.
// It is a result about the release, not a failure of the client.
type ProbeRefusal struct {
	Reason string
	Detail string
}

func (e *ProbeRefusal) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("release probe: %s (%s)", e.Reason, e.Detail)
	}
	return "release probe: " + e.Reason
}

// PlaybackDeviceSource is an optional notifier capability (ADR 0026 §5): the
// playback devices a media server knows and what each decodes. plurx answers
// it from the capabilities its clients report with every playback decision.
type PlaybackDeviceSource interface {
	PlaybackDevices(ctx context.Context) ([]PlaybackDevice, error)
}

// PlaybackDevice is one signed-in playback device.
type PlaybackDevice struct {
	ID       string
	User     string
	Device   string
	Client   string
	LastSeen int64
	Caps     hdr.DeviceCaps
}
