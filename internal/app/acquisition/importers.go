package acquisition

import (
	"context"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/app/transfers"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// The data plane runs here, off the control loop.
//
// It used to run inside `queue.refresh`: the sweep called the download
// client, then imported whatever had finished, on the same goroutine. A 20 GB
// import across a network mount held the poll open for seventeen minutes, and
// every control-plane signal that reads that loop reported a file copy
// instead of a connection. The Connections panel degraded the client it was
// importing FROM, because the contact clock could not advance while the poll
// could not tick — a status page describing its own scheduler.
//
// So the poll's job is now only to notice. Deciding still happens inline
// (it is cheap and must stay ordered), but the moment a decision means moving
// bytes, that work is handed to these workers and the sweep returns. The poll
// now takes milliseconds whatever the library is doing.
const (
	// importWorkers bounds concurrent imports.
	//
	// Two, not more: imports are almost always IO-bound on one filesystem or
	// one network mount, and running eight of them at once makes each slower
	// while making the disk seek harder. It is a throughput knob with a
	// wrong-way-round curve past the low single digits.
	importWorkers = 2
	// importQueue is how many imports may be waiting.
	//
	// Bounded on purpose. If it fills, the enqueue is dropped and the next
	// 30-second sweep will see the same completed download and offer it
	// again — the poll is idempotent, so a full queue costs latency rather
	// than correctness. An unbounded channel would trade that for unbounded
	// memory, which is a worse deal.
	importQueue = 64
)

// importJob is one download ready to be moved into the library.
type importJob struct {
	dl  sqlite.Download
	cfg ports.ClientConfig
}

// StartImporters launches the import workers. Cancel ctx to stop them, then
// WaitImporters to drain — the same lifecycle the jobs queue uses, so there
// is one shutdown idiom in this process rather than two.
func (s *Service) StartImporters(ctx context.Context) {
	s.importOnce.Do(func() {
		s.importCh = make(chan importJob, importQueue)
	})
	for i := 0; i < importWorkers; i++ {
		s.importWG.Add(1)
		go func() {
			defer s.importWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-s.importCh:
					if !ok {
						return
					}
					s.runImportTracked(ctx, job)
				}
			}
		}()
	}
	s.log.Info("import workers started", "workers", importWorkers)
}

// WaitImporters blocks until every worker has stopped.
func (s *Service) WaitImporters() { s.importWG.Wait() }

// Transfers is the in-flight data-plane view, for the API and the metrics.
func (s *Service) Transfers() []transfers.Transfer {
	return s.registry.Snapshot()
}

// LiveStage is what is happening to one download right now, for its queue
// row. Not in flight — no stage, and the row shows its persisted state alone.
func (s *Service) LiveStage(download int64) (transfers.Transfer, bool) {
	return s.registry.Stage(download)
}

// SetRegistry wires the in-flight registry. Optional: without one, every
// report is a no-op and the pipeline behaves exactly as it did.
func (s *Service) SetRegistry(r *transfers.Registry) { s.registry = r }

// enqueueImport hands an import to the workers, or runs it inline when the
// workers are not running.
//
// The inline path is not a fallback for production — it is what keeps every
// existing test, and ManualImport, working unchanged. A test that calls
// RefreshQueue and then asserts the row is `imported` is asserting something
// true and should not have to learn about workers to keep doing so.
func (s *Service) enqueueImport(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig) {
	if s.importCh == nil {
		s.runImportTracked(ctx, importJob{dl: dl, cfg: cfg})
		return
	}
	select {
	case s.importCh <- importJob{dl: dl, cfg: cfg}:
	default:
		// Full. The next sweep sees the same completed download and offers
		// it again, so this costs 30 seconds and nothing else.
		s.log.Warn("import queue full; deferring to the next sweep",
			"download", dl.ID, "release", dl.ReleaseTitle)
	}
}

// runImportTracked imports one download and reports it as in flight for the
// whole of it — which is the entire point: before this, a running import was
// visible nowhere at all.
func (s *Service) runImportTracked(ctx context.Context, job importJob) {
	h := s.registry.Begin(transfers.Transfer{
		DownloadID: job.dl.ID,
		Transfer:   job.dl.Transfer,
		Title:      job.dl.ReleaseTitle,
		Stage:      transfers.StageImporting,
		Total:      job.dl.Size,
		Detail:     "placing files in the library",
	})
	defer h.End()
	_ = s.runImport(transfers.WithHandle(ctx, h), job.dl)
}

// importerFields is the state StartImporters needs. Kept beside the workers
// rather than in the Service literal so the two are read together.
type importerFields struct {
	importOnce sync.Once
	importCh   chan importJob
	importWG   sync.WaitGroup
	registry   *transfers.Registry
}

// watchDownloading keeps the in-flight view honest about the stages Monarr
// does not perform: nzbd is doing the work, and this is Monarr reporting what
// it was told rather than what it did.
//
// `ppStage` is nzbd's own post-processing stage when the observation carried
// one. Monarr used to drop it — a job_pp_stage event arrived saying
// `par_repair`, the reconciler saw StateDownloading in the status beside it,
// and the stage name went nowhere. So a release spending twenty minutes
// repairing a damaged archive was reported as "downloading" the entire time,
// which is not a summary of what was happening, it is a different claim.
func (s *Service) watchDownloading(dl sqlite.Download, st ports.DownloadStatus, ppStage string) {
	if s.registry == nil {
		return
	}
	switch st.State {
	case ports.StateQueued, ports.StateDownloading:
		stage := transfers.StageDownloading
		if ppStage != "" {
			stage = ppStage
		}
		h := s.registry.Begin(transfers.Transfer{
			DownloadID: dl.ID,
			Transfer:   dl.Transfer,
			Title:      dl.ReleaseTitle,
			Stage:      stage,
			Peer:       "nzbd",
			Total:      dl.Size,
			Detail:     st.Message,
		})
		// Only the fetch has a meaningful byte count. Post-processing reports
		// no total, and a percentage invented for it would be a guess wearing
		// a progress bar.
		if stage == transfers.StageDownloading && dl.Size > 0 && st.Progress > 0 {
			h.Bytes(int64(st.Progress*float64(dl.Size)), dl.Size)
		}
	default:
		// Completed, failed, or gone: nothing of this job is in flight at
		// nzbd any more. Ending a stage that was never begun is a no-op, so
		// this needs no guard — but it must clear every nzbd-side stage, not
		// just downloading, or a job that fails during unpack leaves an
		// "extracting" row behind forever.
		s.registry.EndClientStages(dl.ID)
	}
}

// StalledImports is how long the longest-running import has been going, for
// the health check. A data-plane question, answered by data-plane state —
// never again by watching whether a scheduler ticked.
func (s *Service) StalledImports() (time.Duration, bool) {
	return s.registry.Oldest(transfers.StageImporting)
}
