package acquisition

import (
	"context"
	"encoding/json"
	"github.com/pjunod/monarr/internal/adapters/nzbd"
	"github.com/pjunod/monarr/internal/ports"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReviewCompletionAfterMissedResume(t *testing.T) {
	for _, cause := range []string{"capacity", "quota", "post_failure", "storage_unavailable", "missing_tool", "unknown"} {
		for _, mode := range []string{"queue-and-history", "retired-queue", "queue-resolution-only"} {
			t.Run(cause+"/"+mode, func(t *testing.T) {
				svc, db, _ := setup(t, nil, &fakeClient{})
				ctx := context.Background()
				id := grabEpisode(t, svc)
				dl, _ := db.GetDownload(ctx, id)
				held := ports.DownloadControl{Version: 1, Revision: "42", Lifecycle: "held", Cause: cause, Stage: "download_write", RetryPolicy: "resume_same_job", Instance: "same"}
				raw, _ := json.Marshal(held)
				dl.RunnerControl = string(raw)
				dl.State = "downloading"
				if err := db.UpdateDownloadControl(ctx, dl); err != nil {
					t.Fatal(err)
				}
				running := held
				running.Revision = "43"
				running.Lifecycle = "running"
				resolved, _ := json.Marshal(running)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/jobs" {
						jobs := []any{}
						if mode != "retired-queue" {
							jobs = append(jobs, map[string]any{"id": 1, "status": "completed", "pp_done": true, "ready": true, "control": running})
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
						return
					}
					params := [][]string{}
					if mode != "queue-resolution-only" {
						params = append(params, []string{"*Control:v1", string(resolved)})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"entries": []any{map[string]any{"job": 1, "status": "SUCCESS", "final_dir": "/completed/payload", "params": params}}})
				}))
				defer srv.Close()
				client := nzbd.New(ports.ClientConfig{URL: srv.URL})
				statuses, err := client.Statuses(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(statuses) != 1 {
					t.Fatalf("statuses %v", statuses)
				}
				st := statuses[0]
				if !svc.acceptDownloadControl(ctx, &dl, &st) {
					t.Fatalf("completed job remains permanently held after missed resume: status=%+v stored=%s", st, dl.RunnerControl)
				}
				if st.State != ports.StateCompleted || persistedDownloadHeld(dl.RunnerControl) {
					t.Fatalf("completion failed to resolve hold: %+v %+v", dl, st)
				}
			})
		}
	}
}
func TestReviewRunningProgressWithSameControlRevision(t *testing.T) {
	svc, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, _ := db.GetDownload(ctx, id)
	control := ports.DownloadControl{Version: 1, Revision: "43", Lifecycle: "running", Cause: "capacity", Stage: "download_write", Instance: "same"}
	first := ports.DownloadStatus{State: ports.StateDownloading, Progress: 0.2, Control: &control}
	if !svc.acceptDownloadControl(ctx, &dl, &first) {
		t.Fatal("initial control rejected")
	}
	next := first
	next.Progress = 0.8
	if !svc.acceptDownloadControl(ctx, &dl, &next) {
		t.Fatal("unchanged control revision discards fresh download progress")
	}
	persisted, err := db.GetDownload(ctx, id)
	if err != nil || persisted.Progress != 0.8 {
		t.Fatalf("fresh progress not stored: %+v %v", persisted, err)
	}
	for _, change := range []string{"cause", "stage", "message", "instance", "lifecycle", "revision"} {
		conflicting := control
		switch change {
		case "cause":
			conflicting.Cause = "quota"
		case "stage":
			conflicting.Stage = "extract"
		case "message":
			conflicting.Message = "conflicting control"
		case "instance":
			conflicting.Instance = "another"
		case "lifecycle":
			conflicting.Lifecycle = "held"
		case "revision":
			conflicting.Revision = "42"
		}
		stale := next
		stale.Control = &conflicting
		if svc.acceptDownloadControl(ctx, &dl, &stale) {
			t.Fatalf("accepted %s conflict at same/older revision", change)
		}
	}
}

func TestReviewHoldReleaseRequiresCurrentMatchingAuthority(t *testing.T) {
	for _, scenario := range []string{"missing", "older", "same", "instance", "cause", "stage", "version", "not-running", "failed-row", "imported-row"} {
		t.Run(scenario, func(t *testing.T) {
			svc, db, _ := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			id := grabEpisode(t, svc)
			dl, _ := db.GetDownload(ctx, id)
			held := ports.DownloadControl{Version: 1, Revision: "12", Lifecycle: "held", Cause: "post_failure", Stage: "post_unpack_rename", RetryPolicy: "review", Instance: "same-job"}
			raw, _ := json.Marshal(held)
			dl.RunnerControl, dl.State = string(raw), "downloading"
			next := held
			next.Revision, next.Lifecycle = "13", "running"
			status := ports.DownloadStatus{State: ports.StateCompleted, Progress: 1, Control: &next}
			switch scenario {
			case "missing":
				status.Control = nil
			case "older":
				next.Revision = "11"
			case "same":
				next.Revision = "12"
			case "instance":
				next.Instance = "restored-different-job"
			case "cause":
				next.Cause = "capacity"
			case "stage":
				next.Stage = "move"
			case "version":
				next.Version = 2
			case "not-running":
				next.Lifecycle = "succeeded"
			case "failed-row":
				dl.State = "failed"
			case "imported-row":
				dl.State = "imported"
			}
			svc.acceptDownloadControl(ctx, &dl, &status)
			if !persistedDownloadHeld(dl.RunnerControl) {
				t.Fatalf("%s cleared retained custody without matching release authority: %s", scenario, dl.RunnerControl)
			}
		})
	}
}
