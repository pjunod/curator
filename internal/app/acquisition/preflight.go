package acquisition

import (
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/acquisitionplan"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// Pre-flight probing (ADR 0031 §6): before a release is grabbed, Runner reads
// the head of its main file and Curator measures those bytes with the same
// prober it runs on imported files. The measurement replaces the name's HDR
// claim in the decision and the ranking; a release Runner cannot peek at
// keeps its claim and ranks below every measured one of the same tier.

// preflightConcurrency bounds how many probes run at once: each is a handful
// of NNTP articles, and Runner shares its servers with real downloads.
const preflightConcurrency = 3

// preflightTimeout bounds the whole pre-flight pass of one search.
const preflightTimeout = 2 * time.Minute

// probeCacheTTL is how long a probe's answer about a release stands. A post
// does not change, so a measurement or a release-attributable refusal is
// reused: an item searched every few hours does not fetch the same NZBs from
// the indexer and read the same articles again. Failures (Runner or the
// network did not answer) are never cached.
const (
	probeCacheTTL = 24 * time.Hour
	probeCacheMax = 4096
)

// probeCache remembers what pre-flight learned about a release, keyed by the
// release itself (indexer, GUID, title, size) rather than its URL, which
// carries an API key and can change.
type probeCache struct {
	mu      sync.Mutex
	entries map[string]probeCacheEntry
}

type probeCacheEntry struct {
	ev *ProbeEvidence
	at time.Time
}

func probeKey(r ports.Release) string {
	return r.Indexer + "\x00" + r.GUID + "\x00" + r.Title + "\x00" + strconv.FormatInt(r.Size, 10)
}

func (c *probeCache) get(r ports.Release, now time.Time) *ProbeEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[probeKey(r)]
	if !ok || now.Sub(e.at) > probeCacheTTL {
		return nil
	}
	return e.ev
}

func (c *probeCache) put(r ports.Release, ev *ProbeEvidence, now time.Time) {
	if !ev.measured() && !ev.aboutRelease() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]probeCacheEntry{}
	}
	if len(c.entries) >= probeCacheMax {
		for k, e := range c.entries {
			if now.Sub(e.at) > probeCacheTTL || len(c.entries) >= probeCacheMax {
				delete(c.entries, k)
			}
		}
	}
	c.entries[probeKey(r)] = probeCacheEntry{ev: ev, at: now}
}

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

// aboutRelease reports whether an unmeasured probe says something about the
// release itself: it is encrypted, compressed, incomplete or holds no media.
// Only that can refuse it under SkipUnreadable. "unreadable" (which Runner
// also uses when no news server answered), a timeout, a Runner error or a
// cancelled pass is about the path to the release, and a release is never
// refused for it: with no working probe, search behaves as it did without
// one (ADR 0031 §6).
func (e *ProbeEvidence) aboutRelease() bool {
	if e == nil || e.Status != ProbeUnavailable {
		return false
	}
	switch e.Reason {
	case ports.ProbeEncrypted, ports.ProbeCompressed, ports.ProbeMissingArticles, ports.ProbeNoMedia:
		return true
	}
	return false
}

// refusesUnder reports whether a probe result refuses its release under the
// profile.
func (e *ProbeEvidence) refusesUnder(p quality.Profile) bool {
	return p.Preflight.SkipUnreadable && e.aboutRelease()
}

// releaseProber returns the client pre-flight probes go through: the first
// enabled usenet client that can probe. A client that cannot (SABnzbd, or a
// Runner too old to advertise release-probe-v1) is passed over rather than
// ending the search: the probe only reads, so it need not be the client a
// grab would use.
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
			continue
		}
		if supported, err := prober.SupportsReleaseProbe(ctx); err != nil || !supported {
			continue
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
		if ev := s.probes.get(r, time.Now()); ev != nil {
			out[i] = ev
			continue
		}
		wg.Add(1)
		go func(i int, r ports.Release) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return // the pass ran out of time: not probed, not refused
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			head, err := prober.ProbeRelease(ctx, r.DownloadURL, maxBytes)
			ev := evidenceOf(head, err)
			ev.Millis = time.Since(started).Milliseconds()
			s.probes.put(r, ev, time.Now())
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
// is known to be FEL and this release measured MEL (ADR 0031 §6).
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
		// Only a release that is this title can say what its disc is.
		if c.Match.Matched {
			s.recordDiscMaster(ctx, item.ID, ev)
		}
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
		if ev.refusesUnder(profile) {
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

// preflightOne is pre-flight for a grab that does not go through a search
// ranking (RSS, season packs): when the profile asks for measurement and the
// name's HDR is not sure, the release is probed and decided on what it
// measures. It reports whether the grab may go ahead and, when it may not,
// why. A probe that could not run leaves the release to its name, as
// search does.
func (s *Service) preflightOne(ctx context.Context, w domain.Wantable, profile quality.Profile,
	r ports.Release, p parser.Parsed) (bool, decision.Rejection) {
	_, bytes, ok := profile.Probes()
	if !ok || r.Protocol != ports.ProtocolUsenet || hdr.Claimed(p.HDR).Sure() {
		return true, decision.Rejection{}
	}
	prober, ok := s.releaseProber(ctx)
	if !ok {
		return true, decision.Rejection{}
	}
	ev := s.probeReleases(ctx, prober, []ports.Release{r}, bytes)[0]
	if ev == nil {
		return true, decision.Rejection{}
	}
	if !ev.refusesUnder(profile) && !ev.measured() {
		return true, decision.Rejection{}
	}
	if ev.refusesUnder(profile) {
		return false, decision.Rejection{Code: CodeProbeUnreadable, Reason: unreadableReason(ev)}
	}
	s.recordDiscMaster(ctx, w.MediaItemID(), ev)
	if d := decision.Decide(measuredRelease(p, ev), w, profile); !d.Accepted {
		if len(d.Rejections) > 0 {
			return false, d.Rejections[0]
		}
		return false, decision.Rejection{Code: decision.CodeHDRRejected, Reason: "declined on the measured HDR"}
	}
	return true, decision.Rejection{}
}

// preflightPlan measures the best few usenet candidates of a season plan
// whose names do not state their HDR, and returns the candidates with the
// measurement in their preference (or without those the measurement or a
// release-attributable refusal rules out).
func (s *Service) preflightPlan(ctx context.Context, profile quality.Profile, cands []acquisitionplan.Candidate,
	releases map[acquisitionplan.CandidateKey]ports.Release, parsed map[acquisitionplan.CandidateKey]parser.Parsed,
	wants map[acquisitionplan.CandidateKey]domain.Wantable, itemID int64) []acquisitionplan.Candidate {
	top, bytes, ok := profile.Probes()
	if !ok || len(cands) == 0 {
		return cands
	}
	var picked []int
	for i, c := range cands {
		if releases[c.Key].Protocol == ports.ProtocolUsenet && !hdr.Claimed(parsed[c.Key].HDR).Sure() {
			picked = append(picked, i)
		}
	}
	if len(picked) == 0 {
		return cands
	}
	prober, ok := s.releaseProber(ctx)
	if !ok {
		return cands
	}
	sort.SliceStable(picked, func(a, b int) bool {
		x, y := cands[picked[a]].Pref, cands[picked[b]].Pref
		if x.Class != y.Class {
			return x.Class > y.Class
		}
		if x.QualityRank != y.QualityRank {
			return x.QualityRank > y.QualityRank
		}
		if x.HDRRank != y.HDRRank {
			return x.HDRRank < y.HDRRank
		}
		return x.FormatScore > y.FormatScore
	})
	if len(picked) > top {
		picked = picked[:top]
	}
	rels := make([]ports.Release, len(picked))
	for k, i := range picked {
		rels[k] = releases[cands[i].Key]
	}
	evidence := s.probeReleases(ctx, prober, rels, bytes)
	drop := map[int]bool{}
	for k, i := range picked {
		ev, c := evidence[k], &cands[i]
		switch {
		case ev == nil:
		case ev.refusesUnder(profile):
			drop[i] = true
		case ev.measured():
			s.recordDiscMaster(ctx, itemID, ev)
			if !decision.Decide(measuredRelease(parsed[c.Key], ev), wants[c.Key], profile).Accepted {
				drop[i] = true
				continue
			}
			c.Pref.HDRRank, c.Pref.HDRMeasured = profile.HDRRank(ev.Tier), true
		}
	}
	if len(drop) == 0 {
		return cands
	}
	kept := make([]acquisitionplan.Candidate, 0, len(cands)-len(drop))
	for i, c := range cands {
		if !drop[i] {
			kept = append(kept, c)
		}
	}
	return kept
}
