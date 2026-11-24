package acquisition

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type ActivityAction struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}
type ActivityOperationRef struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	State   string `json:"state"`
	Message string `json:"message"`
}
type ActivityActions struct {
	Operation    *ActivityOperationRef   `json:"operation,omitempty"`
	Identity     sqlite.ActivityIdentity `json:"identity"`
	Reason       string                  `json:"reason"`
	Prerequisite string                  `json:"prerequisite,omitempty"`
	Actions      []ActivityAction        `json:"actions"`
}

func (a ActivityActions) allows(kind string) bool {
	for _, action := range a.Actions {
		if action.Kind == kind && action.Enabled {
			return true
		}
	}
	return false
}

func (s *Service) ActivityActions(ctx context.Context, dl sqlite.Download) (ActivityActions, error) {
	out := ActivityActions{Identity: sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"}, Actions: []ActivityAction{}}
	a, e := s.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if e == nil {
		out.Identity.Epoch = strconv.FormatInt(a.Epoch, 10)
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	requested, verified, e := s.db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey)
	if e != nil {
		return out, e
	}
	if requested {
		var ref ActivityOperationRef
		if err := s.db.R.QueryRowContext(ctx, `SELECT o.id,o.action,t.state,COALESCE(json_extract(t.payload,'$.message'),'') FROM activity_operations o JOIN activity_operation_targets t ON t.operation_id=o.id WHERE t.download_id=? AND t.generation_key=? AND o.action='delete' ORDER BY o.created_at DESC LIMIT 1`, dl.ID, dl.GenerationKey).Scan(&ref.ID, &ref.Action, &ref.State, &ref.Message); err == nil {
			out.Operation = &ref
		}
		if verified {
			out.Reason = "Deleted. Downloaded and temporary job files removed."
		} else {
			out.Reason = "Deletion is waiting for cleanup to finish."
			if out.Operation != nil && out.Operation.Message != "" {
				out.Reason = out.Operation.Message
				if out.Operation.State == "blocked" {
					out.Actions = append(out.Actions, ActivityAction{Kind: "archive", Label: "Clear from attention (keep files)", Enabled: true})
				}
			}
			out.Actions = append(out.Actions, ActivityAction{Kind: "retry", Label: "Retry deletion", Enabled: true})
		}
		return out, nil
	}
	out.Actions = append(out.Actions, ActivityAction{Kind: "delete", Label: "Delete", Enabled: true})
	if s.ImportRunning(dl.ID) {
		out.Reason = "Import is running."
		return out, nil
	}
	var control ports.DownloadControl
	if dl.RunnerControl != "" && json.Unmarshal([]byte(dl.RunnerControl), &control) == nil && control.Held() {
		if dl.Transfer != "" && dl.ClientID != 0 {
			out.Actions = append(out.Actions, ActivityAction{Kind: "recheck", Label: "Check Runner receipt", Enabled: true})
		}
		out.Reason = "Runner is waiting for an operator action."
		out.Prerequisite = "Open the Runner job to resolve its recorded hold."
		switch control.Cause {
		case "capacity", "allocation":
			out.Reason = "Runner could not allocate space for this download."
			out.Prerequisite = "Free space on Runner's download drive, then retry."
		case "quota":
			out.Reason = "Runner reached its storage quota."
			out.Prerequisite = "Increase or free the quota, then retry."
		case "mount_unavailable", "storage_unavailable":
			out.Reason = "Runner's download storage is unavailable."
			out.Prerequisite = "Reconnect its original drive, then retry."
		case "password_required":
			out.Reason = "The archive needs a password."
			out.Prerequisite = "Enter the password on this job in Runner, then retry."
		case "missing_tool":
			out.Reason = "Runner is missing a required processing tool."
			out.Prerequisite = "Install the required tool shown on the Runner job, then retry."
		}
		if control.ManualPause {
			out.Reason = "Paused in Runner."
			out.Prerequisite = "Resume the job in Runner when you want it to continue."
		}
		if control.Version != 1 {
			out.Reason = "This job uses an unsupported Runner control version."
			out.Prerequisite = "Update Curator to understand this Runner version."
		}
		if control.Version == 1 && !control.ManualPause && (control.RetryPolicy == "resume_same_job" || control.RetryPolicy == "scheduled_retry" || control.RetryPolicy == "reobserve_mount") {
			out.Actions = append(out.Actions, ActivityAction{Kind: "retry", Label: "Retry", Enabled: true})
		}
		return out, nil
	}
	var choice bool
	if e = s.db.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publications p JOIN publication_files f ON f.publication_id=p.id WHERE p.download_id=? AND p.state!='removed' AND f.outcome='unresolved' AND f.reason='not selected for import; place or discard it in Resolve')`, dl.ID).Scan(&choice); e != nil {
		return out, e
	}
	if choice {
		out.Reason = "Some files were left out of the import you approved."
		out.Prerequisite = "Choose the files and destination to import, or delete this job's remaining files."
		out.Actions = append(out.Actions, ActivityAction{Kind: "choose_files", Label: "Choose files", Enabled: true})
		return out, nil
	}

	if dl.State == "failed" || dl.State == "downloaded" || a.Outcome == "cancelled" {
		out.Actions = append(out.Actions, ActivityAction{Kind: "archive", Label: "Move to History (keep files)", Enabled: true})
	}
	out.Reason = ""
	if dl.Error != "" {
		out.Reason = "The import stopped before it finished."
		out.Prerequisite = dl.Error
	}

	switch {
	case strings.Contains(dl.Error, "no library folder"):
		out.Reason = "No library folder is assigned."
		out.Prerequisite = "Assign a library folder, then retry."
	case dl.State == "awaiting_import":
		out.Reason = "The download is ready and waiting for import approval."
		out.Actions = append(out.Actions, ActivityAction{Kind: "approve", Label: "Approve import", Enabled: true})
		return out, nil
	case a.Outcome == "cancelled" || a.HeldStages > 0:
		out.Reason = "Copy stopped before completion."
		out.Prerequisite = "Retry checks the download and restarts the unfinished import."
	case a.PendingPublications > 0 || a.RetainedPublications > 0:
		out.Reason = "An interrupted import needs to finish reconciling its files."
		out.Prerequisite = "Retry checks the recorded files before continuing."
	case dl.State == "imported" || dl.State == "notified":
		out.Reason = "Added to your library."
	case dl.State == "downloading" || dl.State == "grabbed" || dl.State == "submitting" || dl.State == "planned":
		out.Reason = "Download is in progress."
	case out.Reason == "":
		out.Reason = "This job stopped before import completed."
	}
	if dl.State != "downloading" && dl.State != "grabbed" && dl.State != "submitting" && dl.State != "planned" && dl.State != "imported" && dl.State != "notified" && (dl.ImportPath != "" || dl.SavePath != "") {
		out.Actions = append(out.Actions, ActivityAction{Kind: "retry", Label: "Retry", Enabled: true})
	}
	return out, nil
}

func RegisterActivityActionJobs[H ~func(context.Context, domain.Job) error](s *Service, register func(string, H) error) error {
	return register(sqlite.ActivityActionJobKind, H(s.handleActivityAction))
}
func (s *Service) handleActivityAction(ctx context.Context, job domain.Job) error {
	var payload struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal([]byte(job.Payload), &payload); e != nil {
		return e
	}
	o, e := s.db.ActivityOperation(ctx, payload.ID)
	if e != nil {
		return e
	}
	pending := false
	for i := range o.Targets {
		t := &o.Targets[i]
		if t.State == "succeeded" || t.State == "blocked" {
			continue
		}
		t.State = "running"
		t.Message = "Checking this job."
		if e = s.db.SaveActivityTarget(ctx, &o, i); e != nil {
			return e
		}
		dl, err := s.db.GetDownload(ctx, t.ID)
		if err == nil && dl.GenerationKey != t.GenerationKey {
			err = fmt.Errorf("this job generation changed; refresh Activity")
		}
		if err == nil {
			if deleting, _, readErr := s.db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey); readErr != nil {
				err = readErr
			} else if o.Action == "recheck" {
				if deleting {
					err = fmt.Errorf("deletion is already in progress")
				} else {
					err = s.checkActivityReceipt(ctx, dl, t.Epoch)
				}
			} else if o.Action == "archive" {
				if deleting {
					err = s.db.ArchiveBlockedCleanup(ctx, t.ActivityIdentity)
				} else {
					err = s.acknowledgeActivity(ctx, dl, t.ActivityIdentity)
				}
			} else if o.Action == "delete" || (o.Action == "retry" && deleting) {
				t.DispositionRevision, err = s.db.DispositionRevision(ctx, dl.ID, dl.GenerationKey)
				if err == nil {
					err = s.deleteActivityJob(ctx, o.ID, dl, t, &o)
				}
			} else {
				err = s.retryActivityJob(ctx, dl, o.Action, t.Epoch)
			}
		}
		switch {
		case err == nil:
			t.State = "succeeded"
			switch o.Action {
			case "recheck":
				t.Message = "Runner’s terminal receipt was reconciled. Follow the job in Activity."
			case "archive":
				t.Message = "Moved to History. Files and cleanup records kept."
			case "delete":
				t.Message = "Job stopped. Downloaded and temporary files removed. Library media preserved."
			default:
				t.Message = "Retry accepted. Follow its progress in Activity."
			}
		case o.Action != "recheck" && activityPending(err):
			t.State = "pending"
			t.Message = err.Error()
			pending = true
		default:
			t.State = "blocked"
			t.Message = err.Error()
		}
		if t.DispositionRevision > 0 && t.State != "succeeded" {
			if e = s.db.SetDispositionProgress(ctx, *t); e != nil {
				return e
			}
		}
		if e = s.db.SaveActivityTarget(ctx, &o, i); e != nil {
			return e
		}
	}
	if pending {
		return errActivityPending
	}
	return nil
}

var errActivityPending = errors.New("waiting for the file owner to finish deletion")

// checkActivityReceipt uses the same transfer-correlated terminal evidence as
// the background reconciler. An absent receipt cannot prove completion.
func (s *Service) checkActivityReceipt(ctx context.Context, dl sqlite.Download, epoch string) error {
	if dl.RunnerOutcome != "" && !dl.Provisional {
		return nil
	}
	info, err := s.ActivityActions(ctx, dl)
	if err != nil {
		return err
	}
	if info.Identity.Epoch != epoch || !info.allows("recheck") {
		return fmt.Errorf("the job changed; refresh Activity")
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		return err
	}
	feeds, ok := s.newClient(cfg).(ports.RequestAdmissionProtocol)
	if !ok {
		return fmt.Errorf("the download client does not support Runner receipts")
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	adm, found, err := feeds.GetRequestAdmission(lookup, dl.Transfer)
	if err != nil {
		return fmt.Errorf("checking Runner receipt: %w", err)
	}
	if !found {
		return fmt.Errorf("no Runner receipt found for this transfer. Inspect the original job in Runner")
	}
	if adm.RequestToken != dl.Transfer || dl.Handle != "" && adm.JobID != dl.Handle ||
		dl.RunnerInstanceID != "" && adm.RunnerInstanceID != dl.RunnerInstanceID ||
		dl.RunnerAdmissionGeneration != "" && adm.Generation != dl.RunnerAdmissionGeneration {
		return fmt.Errorf("receipt belongs to a different Runner job; inspect the original transfer")
	}
	terminal, ok := admissionTerminal(adm)
	if !ok {
		return fmt.Errorf("no terminal receipt from Runner yet. Use the hold instructions in Activity")
	}
	fact := ports.RunnerFact{Kind: ports.FactTerminal, Transfer: dl.Transfer, Handle: ports.Handle(adm.JobID), Terminal: &terminal, Source: "operator receipt check"}
	if err := s.observe(ctx, dl, cfg, fact); err != nil {
		return err
	}
	fresh, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil {
		return err
	}
	if fresh.GenerationKey != dl.GenerationKey || fresh.Transfer != dl.Transfer {
		return fmt.Errorf("the transfer changed; refresh Activity")
	}
	if fresh.RunnerOutcome != terminal.Outcome || fresh.Provisional {
		return fmt.Errorf("could not confirm this receipt. Inspect the job log and the publication path in Runner")
	}
	return nil
}

func (s *Service) acknowledgeActivity(ctx context.Context, dl sqlite.Download, target sqlite.ActivityIdentity) error {
	info, err := s.ActivityActions(ctx, dl)
	if err != nil {
		return err
	}
	if !info.allows("archive") {
		return fmt.Errorf("this job is no longer stopped; refresh Activity")
	}
	return s.db.AcknowledgeImport(ctx, target)
}

func (s *Service) retryActivityJob(ctx context.Context, dl sqlite.Download, action, epoch string) error {
	info, e := s.ActivityActions(ctx, dl)
	if e != nil {
		return e
	}
	if info.Identity.Epoch != epoch {
		return fmt.Errorf("this import attempt changed; refresh and select it again")
	}
	if !info.allows(action) {
		return fmt.Errorf("this action is no longer available for this job; refresh Activity")
	}
	requested, _, e := s.db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey)
	if e != nil {
		return e
	}
	if requested {
		return fmt.Errorf("deletion was requested; use Retry deletion")
	}
	if dl.State == "awaiting_import" && action != "approve" {
		return fmt.Errorf("approve this import to continue")
	}
	var control ports.DownloadControl
	if dl.RunnerControl != "" && json.Unmarshal([]byte(dl.RunnerControl), &control) == nil && control.Held() {
		return s.ResumeHeldDownload(ctx, dl.ID)
	}
	// A retry reconciles scratch under the same download and title locks before
	// opening the next import epoch. The filesystem proof stays in the service.
	if e = s.reconcileActivityScratch(ctx, dl, false); e != nil {
		return e
	}
	return s.ImportNow(ctx, dl.ID)
}

func (s *Service) deleteActivityJob(ctx context.Context, operation string, dl sqlite.Download, t *sqlite.ActivityTarget, o *sqlite.ActivityOperation) error {
	visited, _ := ctx.Value(activityDispositionVisited{}).(map[string]bool)
	if visited == nil {
		visited = map[string]bool{}
	}
	if visited[dl.GenerationKey] {
		return fmt.Errorf("linked replacement identities form a cycle; deletion stopped")
	}
	visited[dl.GenerationKey] = true
	defer delete(visited, dl.GenerationKey)
	ctx = context.WithValue(ctx, activityDispositionVisited{}, visited)

	// Join derived admissions that had not become independent when Delete
	// committed. Their canonical owner receipt lives on their own disposition.
	children, err := s.db.R.QueryContext(ctx, `SELECT child_id,child_generation FROM replacement_admissions WHERE parent_id=? AND parent_generation=? AND phase='cancel_pending' ORDER BY child_id`, dl.ID, dl.GenerationKey)
	if err != nil {
		return err
	}
	var childIDs []sqlite.ActivityIdentity
	for children.Next() {
		var c sqlite.ActivityIdentity
		if err = children.Scan(&c.ID, &c.GenerationKey); err != nil {
			_ = children.Close()
			return err
		}
		childIDs = append(childIDs, c)
	}
	err = children.Err()
	_ = children.Close()
	if err != nil {
		return err
	}
	for _, identity := range childIDs {
		child, e := s.db.GetDownload(ctx, identity.ID)
		if e != nil {
			return e
		}
		if child.GenerationKey != identity.GenerationKey {
			return fmt.Errorf("linked replacement generation changed")
		}
		ct := sqlite.ActivityTarget{ActivityIdentity: identity}
		ct.DispositionRevision, e = s.db.DispositionRevision(ctx, child.ID, child.GenerationKey)
		if e != nil {
			return e
		}
		if e = s.deleteActivityJob(ctx, operation, child, &ct, o); e != nil {
			ct.State = "blocked"
			ct.Message = e.Error()
			if activityPending(e) {
				ct.State = "pending"
			}
			_ = s.db.SetDispositionProgress(ctx, ct)
			return e
		}
	}
	// A cancellation signal is not writer exit. The canonical download lock is
	// acquired below only after the real worker has returned and closed files.
	if a, e := s.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey); e == nil && a.Outcome != "completed" {
		_, e = s.CancelImportAttempt(ctx, dl.ID, &a.ImportAttemptKey)
		// A crash can leave a committed placement plan awaiting its publication
		// aggregate. Cancellation finalizes that plan; deletion still needs to
		// join its writer and reconcile the publication below.
		if e != nil && !errors.Is(e, sqlite.ErrImportPublicationPending) && !errors.Is(e, sqlite.ErrImportCompleted) {
			return e
		}
	}
	// Join final submission using the same lock as each client Add. All later
	// submissions consult the durable intent, so the post-join identity is stable.
	admissionUnlock := s.lockDownload(dl.ID)
	fresh, readErr := s.db.GetDownload(ctx, dl.ID)
	if readErr == nil && fresh.GenerationKey != dl.GenerationKey {
		readErr = fmt.Errorf("this job generation changed")
	}
	if readErr == nil && (fresh.SubmissionPhase == "pending" || fresh.State == "planned") {
		_, readErr = s.db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='rejected',superseded=1 WHERE id=? AND generation_key=? AND submission_phase='pending'`, fresh.ID, fresh.GenerationKey)
		fresh.SubmissionPhase = "rejected"
	}
	admissionUnlock()
	if readErr != nil {
		return readErr
	}
	dl = fresh
	if dl.ClientID != 0 && dl.Transfer == "" && dl.SubmissionPhase != "rejected" {
		return fmt.Errorf("this older job has no exact client transfer identity; its external files cannot be verified deleted")
	}
	if dl.ClientID != 0 && dl.Transfer != "" && dl.SubmissionPhase != "rejected" {
		cfg, e := s.db.GetDownloadClient(ctx, dl.ClientID)
		if e != nil {
			return e
		}
		client := s.newClient(cfg)
		if isRunnerClient(client) {
			owner, ok := client.(ports.RunnerDispositionProtocol)
			if !ok {
				return fmt.Errorf("update Runner to support verified job deletion")
			}
			admission, ok := client.(ports.RequestAdmissionProtocol)
			if !ok {
				return fmt.Errorf("runner cannot confirm this transfer identity")
			}
			var request ports.RunnerDispositionRequest
			var jobID string
			var canonical string
			if e = s.db.R.QueryRowContext(ctx, `SELECT owner_payload FROM download_dispositions WHERE download_id=? AND generation_key=?`, dl.ID, dl.GenerationKey).Scan(&canonical); e != nil {
				return e
			}
			t.OwnerPayload = canonical
			if t.OwnerPayload == "" {
				adm, found, e := admission.GetRequestAdmission(ctx, dl.Transfer)
				if e != nil {
					return e
				}
				if !found {
					if dl.SubmissionPhase == "uncertain" || dl.SubmissionPhase == "submitting" {
						return errors.Join(errActivityPending, fmt.Errorf("waiting for Runner to settle this exact submission"))
					}
					return fmt.Errorf("runner has no admission receipt for this transfer; restore its records before retrying deletion")
				}
				if (dl.Handle != "" && dl.Handle != adm.JobID) || (dl.RunnerInstanceID != "" && dl.RunnerInstanceID != adm.RunnerInstanceID) || (dl.RunnerAdmissionGeneration != "" && dl.RunnerAdmissionGeneration != adm.Generation) {
					return fmt.Errorf("runner job identity changed; deletion stopped")
				}
				request = ports.RunnerDispositionRequest{RequestID: "curator-delete-" + dl.GenerationKey, Action: "delete", Transfer: dl.Transfer, RunnerInstanceID: adm.RunnerInstanceID, Generation: adm.Generation, SourceArtifactID: adm.SourceArtifactID, SourceGeneration: adm.SourceGeneration}
				b, _ := json.Marshal(struct {
					JobID   string                         `json:"jobId"`
					Request ports.RunnerDispositionRequest `json:"request"`
				}{adm.JobID, request})
				t.OwnerPayload = string(b)
				if _, e = s.db.W.ExecContext(ctx, `UPDATE download_dispositions SET owner_payload=? WHERE download_id=? AND generation_key=? AND owner_payload=''`, t.OwnerPayload, dl.ID, dl.GenerationKey); e != nil {
					return e
				}
				if e = s.db.R.QueryRowContext(ctx, `SELECT owner_payload FROM download_dispositions WHERE download_id=? AND generation_key=?`, dl.ID, dl.GenerationKey).Scan(&canonical); e != nil {
					return e
				}
				if canonical != t.OwnerPayload {
					return errors.Join(errActivityPending, fmt.Errorf("another operation bound this owner's request; retrying the canonical receipt"))
				}
				if index := activityTargetIndex(o, t.ID); index >= 0 {
					if e = s.db.SaveActivityTarget(ctx, o, index); e != nil {
						return e
					}
				}
				jobID = adm.JobID
			} else {
				var saved struct {
					JobID   string                         `json:"jobId"`
					Request ports.RunnerDispositionRequest `json:"request"`
				}
				if e = json.Unmarshal([]byte(t.OwnerPayload), &saved); e != nil {
					return e
				}
				request = saved.Request
				jobID = saved.JobID
			}
			result, e := owner.DisposeJob(ctx, jobID, request)
			if e != nil {
				return e
			}
			if result.State == "pending" {
				return errActivityPending
			}
			if result.State != "verified" {
				return fmt.Errorf("runner could not finish deletion: %s", result.Reason)
			}
			// Feed the owner's actual terminal through the existing reducer. A
			// completed/failed job remains completed/failed after artifact disposal.
			if adm, found, e := admission.GetRequestAdmission(ctx, dl.Transfer); e != nil {
				return e
			} else if found {
				for _, fact := range admissionFacts(adm) {
					if e = s.observe(ctx, dl, cfg, fact); e != nil {
						return e
					}
				}
			}
		} else {
			if err := s.verifyExternalActivityAbsence(ctx, dl, client); err != nil {
				return err
			}
		}
	}
	if e := s.reconcileActivityScratch(ctx, dl, true); e != nil {
		return e
	}
	unlock := s.lockDownload(dl.ID)
	defer unlock()
	pubs, e := s.db.ListPublicationsForDownload(ctx, dl.ID)
	if e != nil {
		return e
	}
	for _, pub := range pubs {
		if pub.State == "removed" {
			continue
		}
		if e = s.cleanupGate(ctx, pub); e != nil {
			return e
		}
		// The persisted Delete authorizes this generation's manifest entries only.
		// Per-file cleanup still checks the live manifest, source and mount identity.
		if _, statErr := os.Lstat(pub.Path); errors.Is(statErr, os.ErrNotExist) {
			if e = s.finishAbsentDispositionPublication(ctx, dl, pub); e != nil {
				return e
			}
			continue
		} else if statErr != nil {
			return statErr
		}
		files, readErr := s.db.ListPublicationFiles(ctx, pub.ID)
		if readErr != nil {
			return readErr
		}
		if publicationNeedsResolution(pub, files) {
			if e = s.reconcileDispositionPublication(ctx, dl, pub); e != nil {
				return e
			}
		}

		if _, e = s.db.ApprovePublicationCleanup(ctx, []int64{pub.ID}); e != nil {
			return e
		}
		if _, e = s.db.W.ExecContext(ctx, `UPDATE publications SET state=CASE WHEN state IN ('discovered','importing') THEN 'import_failed' ELSE state END,cleanup_paused_at=0 WHERE id=?`, pub.ID); e != nil {
			return e
		}
		result, e := s.cleanupPublicationLocked(context.WithValue(withOperatorCleanup(ctx, "job deleted by operator"), activityDispositionContext{}, true), pub.ID)
		if e != nil {
			return e
		}
		if result.Skipped != "" && result.Skipped != "already removed" {
			return fmt.Errorf("cleanup stopped: %s", result.Skipped)
		}
		if len(result.Kept) > 0 {
			return fmt.Errorf("cleanup could not verify every job file: %s", result.Kept[0])
		}
	}
	// Manual imports grant read authority over their originals; deletion only
	// owns Curator's scratch/publication. Originals are deliberately untouched.
	if dl.DeliveryExpected && dl.Protocol != "manual" && dl.ClientID != 0 && len(pubs) == 0 && (dl.ImportPath != "" || dl.SavePath != "") && dl.RunnerOutcome == ports.OutcomeCompleted {
		return fmt.Errorf("the completed files have no verified publication manifest; reconnect Runner and retry discovery")
	}
	if e = s.db.VerifyDownloadDeletion(ctx, dl.ID, dl.GenerationKey, t.DispositionRevision); e != nil {
		return errors.Join(errActivityPending, e)
	}
	return s.db.AddHistory(ctx, "job_deleted", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"downloadId": dl.ID, "generation": dl.GenerationKey, "transfer": dl.Transfer, "operationId": operation, "message": "Job data removed; installed Library media preserved"})
}

func (s *Service) reconcileActivityScratch(ctx context.Context, dl sqlite.Download, deleting bool) error {
	unlock, ok := s.tryLockDownload(dl.ID)
	if !ok {
		return errors.Join(errActivityPending, fmt.Errorf("waiting for the import writer to exit"))
	}
	defer unlock()
	stages, e := s.db.UnresolvedPlacementReferences(ctx)
	if e != nil {
		return e
	}
	for _, p := range stages {
		if p.DownloadID != dl.ID || p.SourceGeneration != dl.GenerationKey {
			continue
		}
		if p.ID == "" || p.ImportEpoch == nil {
			continue
		}
		// Duplicate sidecar/stage references are checked against their actual
		// placement once, never treated as a second physical allocation.
		if strings.HasPrefix(p.ID, "placement:") {
			continue
		}
		targetCtx, release := s.lockImportTarget(ctx, p.ItemID)
		if p.State == "prepared" || p.State == "retained" {
			if reconcileErr := s.reconcilePlacement(context.WithValue(targetCtx, activityDispositionContext{}, true), p); reconcileErr != nil && !errors.Is(reconcileErr, errPlacementNotPublished) {
				release()
				return errors.Join(errActivityPending, reconcileErr)
			}
			fresh, readErr := s.db.GetPlacement(targetCtx, p.ID)
			if readErr != nil {
				release()
				return readErr
			}
			p = fresh
		}

		if p.State == "detached" && deleting {
			if !s.removeCancelledSupersededScratch(targetCtx, p) {
				release()
				return fmt.Errorf("cleanup cannot verify the original temporary file in %s. Move the job to History to keep the record", filepath.Dir(p.Target))
			}
			_, e = s.db.W.ExecContext(targetCtx, `UPDATE import_placements SET state='rolled_back',data=json_set(data,'$.state','rolled_back'),updated_at=? WHERE id=? AND state='detached'`, time.Now().UnixMilli(), p.ID)
		} else if p.State == "cleaned" || p.State == "rolled_back" || p.State == "detached" {
			e = nil
		} else if p.State == "committed" {
			e = s.cleanupPlacement(targetCtx, p)
		} else if _, backupErr := os.Lstat(p.Backup); p.Backup != "" && !errors.Is(backupErr, os.ErrNotExist) {
			// Existing placement reconciliation preserves/recovers the prior Library
			// file. We never reinterpret a backup as disposable download scratch.
			if reconcileErr := s.reconcilePlacement(context.WithValue(targetCtx, activityDispositionContext{}, true), p); reconcileErr != nil && !errors.Is(reconcileErr, errPlacementNotPublished) {
				release()
				return errors.Join(errActivityPending, reconcileErr)
			}
			fresh, readErr := s.db.GetPlacement(targetCtx, p.ID)
			if readErr != nil {
				e = readErr
			} else if fresh.State != "committed" && fresh.State != "cleaned" && fresh.State != "rolled_back" {
				e = s.restoreActivityPrior(targetCtx, p)
				if e == nil {
					if !s.removeCancelledSupersededScratch(targetCtx, p) {
						e = fmt.Errorf("temporary file identity changed during prior-file recovery")
					} else {
						_, e = s.db.W.ExecContext(targetCtx, `UPDATE import_placements SET state='rolled_back',data=json_set(data,'$.state','rolled_back'),updated_at=? WHERE id=? AND state IN ('prepared','retained')`, time.Now().UnixMilli(), p.ID)
					}
				}
			}
		} else if p.PublishedFileGeneration != "" {
			e = fmt.Errorf("an installed file is waiting for its original placement metadata; retry reconciliation")
		} else if !deleting && absentUnpublishedDestination(p) {
			// Keep the old object and parent identity as a cleanup obligation.
			// A later delete must still verify it, even after a successful retry.
			_, e = s.db.W.ExecContext(targetCtx, `UPDATE import_placements SET state='detached',data=json_set(data,'$.state','detached'),updated_at=? WHERE id=? AND state IN ('prepared','retained')`, time.Now().UnixMilli(), p.ID)
		} else if s.removeCancelledSupersededScratch(targetCtx, p) {
			_, e = s.db.W.ExecContext(targetCtx, `UPDATE import_placements SET state='rolled_back',data=json_set(data,'$.state','rolled_back'),updated_at=? WHERE id=? AND state IN ('prepared','retained')`, time.Now().UnixMilli(), p.ID)
		} else if _, statErr := os.Lstat(filepath.Dir(p.Target)); errors.Is(statErr, os.ErrNotExist) {
			e = fmt.Errorf("the recorded import directory %s is missing. Move the job to History to keep the cleanup record and any remaining files", filepath.Dir(p.Target))
		} else {
			e = fmt.Errorf("temporary file identity is unproven or its original drive is unavailable; restore the original storage to retry")
		}
		if e == nil {
			// Only a proven terminal placement can settle the revoked authorization.
			// A retry never reactivates the old grant.
			_, e = s.db.W.ExecContext(targetCtx, `UPDATE import_publication_authorizations SET outcome='reconciled',resolution=CASE WHEN (SELECT state FROM import_placements WHERE id=placement_id) IN ('committed','cleaned') THEN 'operator_adopted' ELSE 'operator_disposed' END,resolved_at=? WHERE download_id=? AND generation_key=? AND placement_id=? AND (outcome='pending' OR resolution='retained_for_review') AND EXISTS(SELECT 1 FROM import_placements p WHERE p.id=placement_id AND p.state IN ('committed','cleaned','rolled_back','detached'))`, time.Now().UnixMilli(), dl.ID, dl.GenerationKey, p.ID)
		}
		release()
		if e != nil {
			return e
		}
	}
	// A journaled stage without a placement must be bound to its exact object.
	// Older records lacking this evidence cannot authorize path-only deletion.
	custody, e := s.db.ImportStageCustody(ctx, dl.ID, dl.GenerationKey)
	if e != nil {
		return e
	}
	for _, stage := range custody {
		if strings.HasPrefix(stage.ID, "stagepath:") {
			if _, e = os.Lstat(stage.Stage); e == nil {
				return fmt.Errorf("an older temporary file lacks a recorded object identity; storage reconciliation is required")
			}
			if !errors.Is(e, os.ErrNotExist) {
				return e
			}
			if e = s.verifyAbsentActivityStage(ctx, dl, stage); e != nil {
				return e
			}
			_, e = s.db.W.ExecContext(ctx, `UPDATE import_attempt_stages SET state='released' WHERE download_id=? AND generation_key=? AND stage=?`, dl.ID, dl.GenerationKey, stage.Stage)
			if e != nil {
				return e
			}
		}
	}
	if a, err := s.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey); err == nil {
		if a.PendingPublications > 0 || a.RetainedPublications > 0 || a.HeldStages > 0 {
			return fmt.Errorf("an interrupted import still has unresolved file identity evidence; restore the original drive and retry reconciliation")
		}
		if a.Outcome == "pending" {
			_, err = s.db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// An explicit retry may abandon an unpublished placement when its entire
// destination directory disappeared on the recorded filesystem. It cannot
// authorize an unlink or certify a deletion, including at a renamed path.
func absentUnpublishedDestination(p sqlite.Placement) bool {
	if p.StorageIdentity == "" || p.StorageRoot != filepath.Dir(p.Target) || p.TargetObserved || p.PreviousSHA256 != "" || p.PublishedFileGeneration != "" || len(p.Superseded) > 0 {
		return false
	}
	dir := p.StorageRoot
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for dir != filepath.Dir(dir) {
		dir = filepath.Dir(dir)
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || rejectSymlinks(dir, false) != nil {
			return false
		}
		storage, err := expectedStorageIdentity(dir)
		return err == nil && storage == p.StorageIdentity
	}
	return false
}

func activityPending(err error) bool {
	if errors.Is(err, errActivityPending) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		code := status.HTTPStatusCode()
		return code >= 500 || code == 408 || code == 429
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

func activityTargetIndex(o *sqlite.ActivityOperation, id int64) int {
	for i, t := range o.Targets {
		if t.ID == id {
			return i
		}
	}
	return -1
}

type activityDispositionVisited struct{}

// A third-party adapter may lack an owner receipt. It can still prove an
// operator's external removal on a pinned completed mount after its exact job
// has left the client. No guessed path or remote numeric handle grants unlink.
func (s *Service) verifyExternalActivityAbsence(ctx context.Context, dl sqlite.Download, client ports.DownloadClient) error {
	statuses, err := client.Statuses(ctx)
	if err != nil {
		return err
	}
	for _, st := range statuses {
		// Duplicates and conflicting handle/transfer bindings are evidence of
		// presence or ambiguity, never evidence that the owner has retired it.
		if (dl.Transfer != "" && st.Transfer == dl.Transfer) || (dl.Handle != "" && string(st.Handle) == dl.Handle) {
			return fmt.Errorf("delete this exact job and its data in the download client, then retry deletion here")
		}
	}
	path := dl.ImportPath
	if path == "" {
		path = dl.SavePath
	}
	if path == "" || !s.safePayloadPath(ctx, downloadRef{ID: dl.ID, ImportPath: path}) {
		return fmt.Errorf("configure a verified completed-folder mapping for this client, then retry deletion")
	}
	if err = s.checkStorageIdentity(ctx, path); err != nil {
		return err
	}
	if !payloadAbsent(path) {
		return fmt.Errorf("remove this job's files in the download client, then retry deletion here")
	}
	return nil
}
