package acquisition

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
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
	dl     sqlite.Download
	cfg    ports.ClientConfig
	ctx    context.Context
	cancel context.CancelFunc
}

// StartImporters launches the import workers. Cancel ctx to stop them, then
// WaitImporters to drain — the same lifecycle the jobs queue uses, so there
// is one shutdown idiom in this process rather than two.
func (s *Service) StartImporters(ctx context.Context) {
	s.importOnce.Do(func() {
		s.importCh = make(chan importJob, importQueue)
		s.importCtx = ctx
		s.importJobs = map[int64]context.CancelFunc{}
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
					s.runImportTracked(job.ctx, job)
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
func (s *Service) enqueueImport(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig) bool {
	if s.importCh == nil {
		s.runImportTracked(ctx, importJob{dl: dl, cfg: cfg})
		return true
	}

	// One queued OR running job per download. This is also the durable-row
	// recovery guard: repeated 30-second observations of an orphaned
	// "importing" row must not enqueue the same payload over and over while it
	// waits behind a large copy.
	s.importMu.Lock()
	if s.importJobs == nil {
		s.importJobs = map[int64]context.CancelFunc{}
	}
	if _, exists := s.importJobs[dl.ID]; exists {
		s.importMu.Unlock()
		return false
	}
	parent := s.importCtx
	if parent == nil {
		parent = ctx
	}
	jobCtx, cancel := context.WithCancel(parent)
	s.importJobs[dl.ID] = cancel
	select {
	case s.importCh <- importJob{dl: dl, cfg: cfg, ctx: jobCtx, cancel: cancel}:
		s.importMu.Unlock()
		return true
	default:
		delete(s.importJobs, dl.ID)
		s.importMu.Unlock()
		cancel()
		// Full. The next sweep sees the same completed download and offers
		// it again, so this costs 30 seconds and nothing else.
		s.log.Warn("import queue full; deferring to the next sweep",
			"download", dl.ID, "release", dl.ReleaseTitle)
		return false
	}
}

// runImportTracked imports one download and reports it as in flight for the
// whole of it — which is the entire point: before this, a running import was
// visible nowhere at all.
func (s *Service) runImportTracked(ctx context.Context, job importJob) {
	if job.cancel != nil {
		defer func() {
			job.cancel()
			s.importMu.Lock()
			delete(s.importJobs, job.dl.ID)
			s.importMu.Unlock()
		}()
	}
	if ctx.Err() != nil {
		return // cancelled while it was still waiting for a worker
	}

	if job.cancel != nil {
		// Restart, cancel, poll and push can all address this row concurrently.
		// An asynchronous copy owns the same per-download lock as reconciliation
		// so there is never a second importer writing underneath it. The inline
		// test path is already called with this lock held by reconciliation.
		unlock := s.lockDownload(job.dl.ID)
		defer unlock()
		fresh, err := s.db.GetDownload(ctx, job.dl.ID)
		if err != nil {
			return
		}
		if fresh.State == "imported" || ctx.Err() != nil {
			return
		}
		// The normal path persisted these before enqueue. Keeping a non-empty
		// value carried by the job also preserves the documented inline/test
		// seam where a caller can hand off a just-observed path directly.
		if job.dl.ImportPath == "" {
			job.dl.ImportPath = fresh.ImportPath
		}
		if job.dl.SavePath == "" {
			job.dl.SavePath = fresh.SavePath
		}
		job.dl.State, job.dl.Handoff = fresh.State, fresh.Handoff
	}
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

// ImportRunning includes work waiting for one of the bounded workers. Live
// transfer state alone cannot see that gap, which is how a queued restart
// could otherwise be mistaken for another orphan and enqueued twice.
func (s *Service) ImportRunning(download int64) bool {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	_, ok := s.importJobs[download]
	return ok
}

// CancelImport stops a queued/running import and returns the row to the
// downloaded state so it can be restarted later. The source payload is left
// alone. Any destination temp file is removed by the copy path.
func (s *Service) CancelImport(ctx context.Context, id int64) error {
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	s.importMu.Lock()
	cancel := s.importJobs[id]
	s.importMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if dl.State != "importing" && cancel == nil {
		return fmt.Errorf("download is not importing")
	}

	// Cancel before waiting for the import's row lock. A long cross-filesystem
	// copy observes the context, removes its temp file, and releases the lock;
	// then this durable transition is the final word.
	unlock := s.lockDownload(id)
	defer unlock()
	dl, err = s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if dl.State == "imported" {
		return fmt.Errorf("import already finished")
	}
	s.advance(ctx, &dl, "downloaded", dl.Progress, "", stepImportCancelled,
		"import cancelled; payload kept and ready to restart")
	return nil
}

// importerFields is the state StartImporters needs. Kept beside the workers
// rather than in the Service literal so the two are read together.
type importerFields struct {
	importOnce sync.Once
	importCh   chan importJob
	importWG   sync.WaitGroup
	registry   *transfers.Registry
	importCtx  context.Context
	importMu   sync.Mutex
	importJobs map[int64]context.CancelFunc
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
