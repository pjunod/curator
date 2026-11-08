package nzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2 (docs/PIPELINE_CONTRACT.md sections 2.1-2.5): the
// adapter reads Runner's terminal, status and publication facts from the
// event stream, the ordered history and the deliveries feed, and hands them
// to the reducer as ports.RunnerFact. A Runner older than M1a sends none of
// the typed fields; its legacy history string is mapped by the rule in
// contract section 6.

var _ ports.RunnerFeeds = (*Client)(nil)

// terminalWire is the typed terminal on job_pp_finished and on a history
// row. Every field is absent on an older Runner.
type terminalWire struct {
	Transfer      string           `json:"transfer"`
	Outcome       string           `json:"outcome"`
	Reason        string           `json:"reason"`
	FetchCause    string           `json:"fetch_cause"`
	Stage         string           `json:"stage"`
	Message       string           `json:"message"`
	BlamesRelease *bool            `json:"blames_release"`
	Salvage       string           `json:"salvage"`
	KeepOnFailure bool             `json:"keep_on_failure"`
	Fetched       *ports.Fetched   `json:"fetched"`
	Publication   *publicationWire `json:"publication"`
}

type publicationWire struct {
	Path           string `json:"path"`
	Generation     string `json:"generation"`
	ManifestDigest string `json:"manifest_digest"`
	Files          int    `json:"files"`
	CrossVolume    bool   `json:"cross_volume"`
}

func (p *publicationWire) port(kind string) *ports.RunnerPublication {
	if p == nil {
		return nil
	}
	return &ports.RunnerPublication{Path: p.Path, Generation: p.Generation, ManifestDigest: p.ManifestDigest,
		Files: p.Files, CrossVolume: p.CrossVolume, Kind: kind}
}

// terminalOf builds the terminal from the typed fields when Runner sent
// them, else from the legacy history string. ok=false for a row that is not
// actionable yet (a SUCCESS without its final path).
func terminalOf(w terminalWire, ppStatus, finalDir string, params [][2]string, record *historyRecord) (ports.RunnerTerminal, bool) {
	var t ports.RunnerTerminal
	if w.Outcome != "" {
		t = ports.RunnerTerminal{Outcome: w.Outcome, Reason: w.Reason, FetchCause: w.FetchCause, Stage: w.Stage,
			Message: w.Message, Salvage: w.Salvage, KeepOnFailure: w.KeepOnFailure, LegacyPPStatus: ppStatus, LegacyFinalDir: finalDir}
		if w.BlamesRelease != nil {
			t.BlamesRelease = *w.BlamesRelease
		} else if w.Outcome == ports.OutcomeFailed {
			// A typed failure without blame: the reason decides (contract 2.2).
			t.BlamesRelease = reasonBlames(w.Reason, w.FetchCause)
		}
		if w.Fetched != nil {
			t.Fetched = *w.Fetched
		}
		if w.Outcome == ports.OutcomeCompleted {
			t.Publication = w.Publication.port("completed")
			if t.Publication == nil && finalDir != "" {
				t.Publication = &ports.RunnerPublication{Path: finalDir, Kind: "completed"}
			}
		}
	} else {
		if ppStatus == "" {
			return t, false
		}
		t = ports.LegacyTerminal(ppStatus, false)
		t.LegacyFinalDir = finalDir
		if t.Outcome == ports.OutcomeCompleted {
			if strings.TrimSpace(finalDir) == "" {
				return t, false
			}
			t.Publication = &ports.RunnerPublication{Path: finalDir, Kind: "completed"}
		}
		if t.Outcome == ports.OutcomeFailed {
			if files := paramValue(params, "Failure:Files"); strings.TrimSpace(files) != "" {
				t.Message += "; files: " + files
			}
		}
	}
	if t.Publication != nil {
		// History rows mirror the manifest identity in their params.
		if t.Publication.Generation == "" {
			t.Publication.Generation = paramValue(params, "Delivery:Generation")
		}
		if t.Publication.ManifestDigest == "" {
			t.Publication.ManifestDigest = paramValue(params, "Delivery:ManifestDigest")
		}
	}
	if !t.Fetched.Known() && record != nil {
		t.Fetched.SuccessArticles, t.Fetched.TotalArticles = record.SuccessArticles, record.TotalArticles
	}
	if t.Salvage == "" {
		t.Salvage = "none"
	}
	return t, true
}

// reasonBlames is contract section 2.2's blame column, for a typed failure
// that did not carry blames_release.
func reasonBlames(reason, fetchCause string) bool {
	switch reason {
	case ports.ReasonHealth, ports.ReasonPar, ports.ReasonUnpack, ports.ReasonIdentity:
		return true
	case ports.ReasonFetch:
		return fetchCause == ports.FetchCauseGone
	}
	return false
}

func transferOf(explicit string, params [][2]string) string {
	if explicit != "" {
		return explicit
	}
	return paramValue(params, TransferParam)
}

// historyFact turns one history row into a terminal fact.
func historyFact(h historyEntry, incarnation string) (ports.RunnerFact, bool) {
	t, ok := terminalOf(h.terminalWire, h.Status, h.FinalDir, h.Params, h.Record)
	if !ok {
		return ports.RunnerFact{}, false
	}
	return ports.RunnerFact{
		Kind: ports.FactTerminal, Transfer: transferOf(h.Transfer, h.Params), Handle: handleOf(h.Job),
		Terminal: &t, Source: fmt.Sprintf("history %s:%d", shortIncarnation(incarnation), h.Seq),
		HistorySeq: h.Seq, HistoryIncarnation: incarnation,
	}, true
}

func shortIncarnation(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "-"
	}
	return s
}

// HistorySince implements ports.HistoryReader over
// `GET /api/v1/history?since_seq=&limit=`. A Runner without an incarnation is
// incarnation "" and never loses continuity by it.
func (c *Client) HistorySince(ctx context.Context, cur ports.HistoryCursor, limit int) ([]ports.RunnerFact, ports.HistoryCursor, bool, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	q := url.Values{}
	q.Set("since_seq", strconv.FormatInt(cur.SinceSeq, 10))
	q.Set("limit", strconv.Itoa(limit))
	var hist statusHistory
	if err := c.do(ctx, http.MethodGet, "/api/v1/history?"+q.Encode(), &hist); err != nil {
		return nil, cur, false, err
	}
	next := ports.HistoryCursor{Incarnation: hist.Incarnation, SinceSeq: cur.SinceSeq, HeadSeq: hist.HeadSeq, RetainedMinSeq: hist.RetainedMinSeq}
	lost := (cur.Incarnation != "" && hist.Incarnation != cur.Incarnation) ||
		(hist.RetainedMinSeq > 0 && cur.SinceSeq+1 < hist.RetainedMinSeq) ||
		(hist.HeadSeq > 0 && cur.SinceSeq > hist.HeadSeq)
	if lost {
		next.SinceSeq = hist.HeadSeq
		return nil, next, true, nil
	}
	entries := append([]historyEntry(nil), hist.Entries...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	facts := make([]ports.RunnerFact, 0, len(entries))
	for _, h := range entries {
		if h.Seq > next.SinceSeq {
			next.SinceSeq = h.Seq
		}
		if (h.Seq > 0 && h.Seq <= cur.SinceSeq) || !c.acceptsTorrentRole(h.Kind, h.TorrentRole) {
			continue
		}
		if f, ok := historyFact(h, hist.Incarnation); ok {
			facts = append(facts, f)
		}
	}
	return facts, next, false, nil
}

// DeliveriesSince implements ports.DeliveriesReader over
// `GET /api/v1/deliveries?after=&include_accepted=true`: a row an old Curator
// acknowledged before placing its files is exactly one this one must still
// see (contract section 2.5). Each row is a completed terminal or, for kind
// salvage (or a failed pre-M1a row), a publication_ready fact.
func (c *Client) DeliveriesSince(ctx context.Context, after uint64, limit int) ([]ports.RunnerFact, uint64, error) {
	if limit < 1 || limit > maxDeliveryPage {
		limit = maxDeliveryPage
	}
	path := "/api/v1/deliveries?after=" + strconv.FormatUint(after, 10) + "&limit=" + strconv.Itoa(limit) + "&include_accepted=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.URL, "/")+path, nil)
	if err != nil {
		return nil, after, err
	}
	page, err := c.listDeliveryPage(req, limit)
	if err != nil {
		return nil, after, err
	}
	facts := make([]ports.RunnerFact, 0, len(page.Deliveries))
	for _, d := range page.Deliveries {
		facts = append(facts, deliveryFact(d))
	}
	return facts, page.NextCursor, nil
}

func deliveryFact(d ports.RunnerDelivery) ports.RunnerFact {
	kind := d.Kind
	if kind == "" {
		// A pre-M1a row: a non-success terminal outcome is a salvage.
		kind = "completed"
		if d.TerminalOutcome != "" && !strings.HasPrefix(d.TerminalOutcome, "SUCCESS") {
			kind = "salvage"
		}
	}
	m := ports.ManifestFromRunnerFiles(d.PublicationGeneration, d.ManifestDigest, d.DeliveryID, d.Files)
	pub := &ports.RunnerPublication{Path: d.PublicationPath, Generation: d.PublicationGeneration, ManifestDigest: d.ManifestDigest,
		Files: len(d.Files), Kind: kind, Manifest: &m}
	f := ports.RunnerFact{Transfer: d.RequestToken, Handle: ports.Handle(d.JobID), Publication: pub,
		Source: fmt.Sprintf("deliveries %d", d.Sequence)}
	if kind == "salvage" {
		f.Kind = ports.FactPublicationReady
		return f
	}
	f.Kind = ports.FactTerminal
	f.Terminal = &ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Salvage: "none", Publication: pub, LegacyPPStatus: d.TerminalOutcome}
	return f
}

// ppFinishedFact reads job_pp_finished.
func ppFinishedFact(data []byte, seq uint64) (ports.RunnerFact, bool) {
	var f struct {
		terminalWire
		Job                int64       `json:"job"`
		PpStatus           string      `json:"pp_status"`
		FinalDir           string      `json:"final_dir"`
		Params             [][2]string `json:"params"`
		HistorySeq         int64       `json:"history_seq"`
		HistoryIncarnation string      `json:"history_incarnation"`
	}
	if json.Unmarshal(data, &f) != nil {
		return ports.RunnerFact{}, false
	}
	t, ok := terminalOf(f.terminalWire, f.PpStatus, f.FinalDir, f.Params, nil)
	if !ok {
		return ports.RunnerFact{}, false
	}
	return ports.RunnerFact{Kind: ports.FactTerminal, Transfer: transferOf(f.Transfer, f.Params), Handle: handleOf(f.Job),
		Terminal: &t, Source: fmt.Sprintf("event %d", seq), HistorySeq: f.HistorySeq, HistoryIncarnation: f.HistoryIncarnation}, true
}

// publicationReadyFact reads job_publication_ready (a salvage).
func publicationReadyFact(data []byte, seq uint64) (ports.RunnerFact, bool) {
	var f struct {
		Job         int64            `json:"job"`
		Transfer    string           `json:"transfer"`
		Publication *publicationWire `json:"publication"`
	}
	if json.Unmarshal(data, &f) != nil || f.Publication == nil || f.Publication.Path == "" {
		return ports.RunnerFact{}, false
	}
	return ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: f.Transfer, Handle: handleOf(f.Job),
		Publication: f.Publication.port("salvage"), Source: fmt.Sprintf("event %d", seq)}, true
}

// statusChangedFact reads job_status_changed.
func statusChangedFact(data []byte, seq uint64) (ports.ClientEvent, bool) {
	var f struct {
		Job      int64                  `json:"job"`
		Transfer string                 `json:"transfer"`
		Status   string                 `json:"status"`
		Stage    string                 `json:"stage"`
		Control  *ports.DownloadControl `json:"control"`
	}
	if json.Unmarshal(data, &f) != nil {
		return ports.ClientEvent{}, false
	}
	st := ports.DownloadStatus{Handle: handleOf(f.Job), Transfer: f.Transfer, Control: f.Control, Stage: f.Stage, State: ports.StateDownloading}
	switch f.Status {
	case "held":
		// held is Runner's operational state, with or without a control
		// object (contract section 2.4 makes control optional).
		st.State, st.OperationalState = ports.StateQueued, "held"
		if f.Control != nil {
			st.Message = f.Control.Message
			if st.Stage == "" {
				st.Stage = f.Control.Stage
			}
		} else {
			st.Message = "held by Runner"
		}
	case "queued", "paused", "fetching":
		st.State = ports.StateQueued
	case "failed":
		// The terminal follows on job_pp_finished; this is not it.
		st.State = ports.StateFailed
		st.Message = "failed (awaiting history)"
	}
	if st.Stage != "" && st.Message == "" {
		st.Message = "post-processing: " + strings.ReplaceAll(st.Stage, "_", " ")
	}
	ev := ports.ClientEvent{Handle: st.Handle, Kind: ports.EventProgress, Seq: seq, Status: st, Stage: f.Stage}
	ev.Fact = statusFact(st, seq)
	return ev, true
}

func statusFact(st ports.DownloadStatus, seq uint64) *ports.RunnerFact {
	s := st
	src := "poll"
	if seq > 0 {
		src = fmt.Sprintf("event %d", seq)
	}
	return &ports.RunnerFact{Kind: ports.FactStatus, Transfer: st.Transfer, Handle: st.Handle, Status: &s, Source: src}
}
