package acquisition

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// Pre-flight probing (ADR 0026 §6): before a release is grabbed, Runner reads
// the head of its main file and Curator measures those bytes with the same
// prober it runs on imported files. The measurement replaces the name's HDR
// claim in the decision and the ranking; a release Runner cannot peek at
// keeps its claim and ranks below every measured one of the same tier.

// preflightConcurrency bounds how many probes run at once: each is a handful
// of NNTP articles, and Runner shares its servers with real downloads.
const preflightConcurrency = 3

// preflightTimeout bounds the whole pre-flight pass of one search.
const preflightTimeout = 2 * time.Minute

// ProbeEvidence is what a pre-flight probe found, for the decision and for
// the evidence panel the search list shows.
type ProbeEvidence struct {
	// Status is "measured", "unavailable" (Runner said why it could not
	// read the head) or "failed" (Runner or the network did not answer).
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// What Runner read.
	File     string   `json:"file,omitempty"`
	Path     []string `json:"path,omitempty"`
	Articles int      `json:"articles,omitempty"`
	Missing  int      `json:"missing,omitempty"`
	Bytes    int      `json:"bytes,omitempty"`
	Millis   int64    `json:"millis,omitempty"`
	// What the bytes measured.
	Container string              `json:"container,omitempty"`
	Facts     string              `json:"facts,omitempty"`
	Tier      hdr.Tier            `json:"tier,omitempty"`
	DoVi      *mediainfo.DoViInfo `json:"dovi,omitempty"`
}

// Probe statuses.
const (
	ProbeMeasured    = "measured"
	ProbeUnavailable = "unavailable"
	ProbeFailed      = "failed"
)

// measured reports whether the probe produced an HDR measurement.
func (e *ProbeEvidence) measured() bool { return e != nil && e.Status == ProbeMeasured }

// releaseProber returns the client pre-flight probes go through: the first
// enabled usenet client, the same one a grab would use, when it can probe.
func (s *Service) releaseProber(ctx context.Context) (ports.ReleaseProber, bool) {
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return nil, false
	}
	for _, cfg := range clients {
		if !cfg.Enabled || protocolOfClient(cfg.Type) != ports.ProtocolUsenet {
			continue
		}
		prober, ok := s.newClient(cfg).(ports.ReleaseProber)
		if !ok {
			return nil, false
		}
		if supported, err := prober.SupportsReleaseProbe(ctx); err != nil || !supported {
			return nil, false
		}
		return prober, true
	}
	return nil, false
}

// probeReleases probes each release, concurrently and bounded, and returns
// the evidence in the same order. A nil entry means the release was not
// probed (not usenet).
func (s *Service) probeReleases(ctx context.Context, prober ports.ReleaseProber, releases []ports.Release, maxBytes int64) []*ProbeEvidence {
	out := make([]*ProbeEvidence, len(releases))
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	sem := make(chan struct{}, preflightConcurrency)
	var wg sync.WaitGroup
	for i, r := range releases {
		if r.Protocol != ports.ProtocolUsenet || r.DownloadURL == "" {
			continue
		}
		wg.Add(1)
		go func(i int, r ports.Release) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			head, err := prober.ProbeRelease(ctx, r.DownloadURL, maxBytes)
			ev := evidenceOf(head, err)
			ev.Millis = time.Since(started).Milliseconds()
			out[i] = ev
			s.log.Debug("preflight: probed", "release", r.Title, "status", ev.Status,
				"reason", ev.Reason, "tier", string(ev.Tier), "bytes", ev.Bytes)
		}(i, r)
	}
	wg.Wait()
	return out
}

// evidenceOf measures what Runner returned.
func evidenceOf(head ports.ReleaseHead, err error) *ProbeEvidence {
	var refusal *ports.ProbeRefusal
	switch {
	case errors.As(err, &refusal):
		return &ProbeEvidence{Status: ProbeUnavailable, Reason: refusal.Reason, Detail: refusal.Detail}
	case err != nil:
		return &ProbeEvidence{Status: ProbeFailed, Detail: err.Error()}
	}
	ev := &ProbeEvidence{File: head.File, Path: head.Path, Articles: head.Articles,
		Missing: head.Missing, Bytes: len(head.Bytes)}
	size := head.FileSize
	if size < int64(len(head.Bytes)) {
		size = int64(len(head.Bytes))
	}
	info, _ := mediainfo.Probe(headReader(head.Bytes), size)
	if !info.Measured() {
		ev.Status, ev.Reason = ProbeUnavailable, ports.ProbeUnreadable
		ev.Detail = "the head Runner read does not parse as a video container"
		return ev
	}
	ev.Status = ProbeMeasured
	ev.Container = info.Container
	ev.Facts = info.Summary()
	ev.Tier = info.HDRTier()
	if info.Video != nil {
		ev.DoVi = info.Video.DoVi
	}
	return ev
}

// headReader serves the bytes Runner read as the start of a file whose rest
// is absent: reads past the head end early, which the probers treat as
// "could not tell", never as an error about the file.
type headReader []byte

func (h headReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(h)) {
		return 0, io.EOF
	}
	n := copy(p, h[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// discMasterNote is the warning a profile 7 remux gets when the title's disc
// is known to be FEL and this release measured MEL (ADR 0026 §6).
func discMasterNote(discEL string, ev *ProbeEvidence) string {
	if !ev.measured() || discEL != hdr.ELFEL || ev.Tier != hdr.TierDVP7MEL {
		return ""
	}
	return "this title's disc carries a full enhancement layer (FEL); this release measures MEL, so it is a different master"
}

// recordDiscMaster stores what a probe showed about the title's disc.
func (s *Service) recordDiscMaster(ctx context.Context, itemID int64, ev *ProbeEvidence) {
	if !ev.measured() {
		return
	}
	el := ""
	switch ev.Tier {
	case hdr.TierDVP7FEL:
		el = hdr.ELFEL
	case hdr.TierDVP7MEL:
		el = hdr.ELMEL
	}
	if el == "" {
		return
	}
	if err := s.db.RecordDiscEnhancementLayer(ctx, itemID, el); err != nil {
		s.log.Warn("preflight: could not record the disc master", "item", itemID, "err", err)
	}
}

// preflightCandidates probes the best-ranked candidates of an interactive
// search (out is already sorted) and re-decides them on the measurement.
func (s *Service) preflightCandidates(ctx context.Context, out []Candidate, item domain.MediaItem,
	target domain.Wantable, profile quality.Profile, top int, bytes int64) {
	prober, ok := s.releaseProber(ctx)
	if !ok {
		return
	}
	var picked []int
	for i := range out {
		if len(picked) >= top {
			break
		}
		if out[i].Accepted && out[i].Release.Protocol == ports.ProtocolUsenet {
			picked = append(picked, i)
		}
	}
	releases := make([]ports.Release, len(picked))
	for k, i := range picked {
		releases[k] = out[i].Release
	}
	evidence := s.probeReleases(ctx, prober, releases, bytes)
	for k, i := range picked {
		ev := evidence[k]
		if ev == nil {
			continue
		}
		c := &out[i]
		c.Probe = ev
		s.recordDiscMaster(ctx, item.ID, ev)
		if note := discMasterNote(item.DiscEL, ev); note != "" {
			c.Warning = joinWarning(c.Warning, note)
		}
		if ev.measured() {
			m := measuredHDR(ev)
			c.MeasuredHDR = &m
			d := decision.Decide(measuredRelease(c.parsed, ev), target, profile)
			c.Accepted, c.IsUpgrade, c.Rejections = d.Accepted, d.IsUpgrade, d.Rejections
			continue
		}
		if profile.Preflight.SkipUnreadable {
			c.Accepted, c.IsUpgrade = false, false
			c.Rejections = append(c.Rejections, decision.Rejection{Code: CodeProbeUnreadable, Reason: unreadableReason(ev)})
		}
	}
}

// CodeProbeUnreadable refuses a release whose head could not be read, under
// a profile that asks to skip those rather than rank them by name.
const CodeProbeUnreadable = "probe_unreadable"

func unreadableReason(ev *ProbeEvidence) string {
	why := ev.Reason
	if why == "" {
		why = ev.Detail
	}
	return "could not measure the release before grabbing (" + why + ") and the profile skips those"
}

// measuredRelease is the decision engine's view of a probed release: the
// name's quality and languages, the measured HDR.
func measuredRelease(p parser.Parsed, ev *ProbeEvidence) decision.Release {
	r := releaseOf(p)
	r.HDR = hdr.Measure(ev.Tier)
	return r
}

func measuredHDR(ev *ProbeEvidence) CandidateHDR {
	c := CandidateHDR{Tier: ev.Tier, Label: ev.Tier.Label(), Measured: true}
	if ev.DoVi != nil {
		c.DVProfile, c.ELType = ev.DoVi.Profile, ev.DoVi.ELType
		c.Label = ev.DoVi.Label()
	}
	return c
}

func joinWarning(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
