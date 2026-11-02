package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestSuccessfulRetryCannotAutomaticallyDeletePriorHeldSource(t *testing.T) {
	for _, kind := range []string{"stage", "adopted-publication"} {
		t.Run(kind, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, itemID := setupWithClientType(t, client, "sabnzbd")
			ctx := context.Background()
			root := t.TempDir()
			payload := filepath.Join(root, "release")
			src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
			if e := os.MkdirAll(payload, 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(src, corpusFile(t, wholeCorpus720), 0644); e != nil {
				t.Fatal(e)
			}
			if e := svc.SetCompletedRoots(ctx, root); e != nil {
				t.Fatal(e)
			}
			if e := svc.ScanCompleted(ctx); e != nil {
				t.Fatal(e)
			}
			dl := insertImportable(t, db, itemID, payload)
			a, e := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "stage" {
				e = db.RecordImportStage(ctx, a.ImportAttemptKey, src, "/library/caption", filepath.Join(root, ".retained-stage"))
			} else {
				_, e = db.W.ExecContext(ctx, `INSERT INTO import_publication_authorizations(download_id,generation_key,attempt_epoch,publication_id,source,target,payload,authorized_at,outcome,resolved_at,resolution) VALUES(?,?,?,'adopted',?,'/library/old',json_object('source',?),0,'reconciled',0,'operator_adopted')`, dl.ID, dl.GenerationKey, a.Epoch, src, src)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
				t.Fatal(e)
			}
			if _, e = db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); e != nil {
				t.Fatal(e)
			}
			if _, e = db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); e != nil {
				t.Fatal(e)
			}
			if e = svc.runImport(ctx, dl); e != nil {
				t.Fatal(e)
			}
			fresh, e := db.GetDownload(ctx, dl.ID)
			if e != nil || fresh.State != "imported" {
				t.Fatalf("retry did not complete: %+v %v", fresh, e)
			}
			if len(client.removed) != 0 {
				t.Fatal("client source removal bypassed prior custody")
			}
			if _, e = os.Stat(src); e != nil {
				t.Fatal("local fallback removed held source", e)
			}
			if e = svc.CleanupPayloads(ctx); e != nil {
				t.Fatal(e)
			}
			if len(client.removed) != 0 {
				t.Fatal("sweep removed held source")
			}
		})
	}
}

func TestSourceCustodyIncludesOverlappingOtherGeneration(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	if e := svc.SetCompletedRoots(ctx, root); e != nil {
		t.Fatal(e)
	}
	id, e := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, Protocol: "manual", State: "downloaded", ReleaseTitle: "old generation"})
	if e != nil {
		t.Fatal(e)
	}
	dl, e := db.GetDownload(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	a, e := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.RecordImportStage(ctx, a.ImportAttemptKey, filepath.Join(root, "nested", "file"), "/library/result", "/library/.stage"); e != nil {
		t.Fatal(e)
	}
	if e = svc.assertImportSourceDisposable(ctx, root); e == nil {
		t.Fatal("tree removal ignored nested other-acquisition custody")
	}
	if e = svc.assertImportSourceDisposable(ctx, root+"-different"); e != nil {
		t.Fatal("unrelated path blocked", e)
	}
}

func TestSourceCustodyCannotBeBypassedThroughBindAlias(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	completed := filepath.Join(root, "completed")
	alias := t.TempDir()
	writeCompleted(t, filepath.Join(completed, "release", "keep.mkv"), 8)
	writeCompleted(t, filepath.Join(alias, "release", "keep.mkv"), 8)
	left, e := os.Stat(completed)
	if e != nil {
		t.Fatal(e)
	}
	right, e := os.Stat(alias)
	if e != nil {
		t.Fatal(e)
	}
	original := storageSameFile
	storageSameFile = func(a, b os.FileInfo) bool {
		return os.SameFile(a, b) || (os.SameFile(a, left) && os.SameFile(b, right)) || (os.SameFile(a, right) && os.SameFile(b, left))
	}
	t.Cleanup(func() { storageSameFile = original })
	if e = svc.SetCompletedRoots(ctx, root+"\n"+alias); e != nil {
		t.Fatal(e)
	}
	id, e := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, Protocol: "manual", State: "downloaded", ReleaseTitle: "bind-alias"})
	if e != nil {
		t.Fatal(e)
	}
	dl, e := db.GetDownload(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	a, e := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.RecordImportStage(ctx, a.ImportAttemptKey, filepath.Join(alias, "release", "keep.mkv"), "/library/result", "/library/.held"); e != nil {
		t.Fatal(e)
	}
	if e = svc.assertImportSourceDisposable(ctx, filepath.Join(completed, "release")); e == nil {
		t.Fatal("bind alias bypassed retained source custody")
	}
}
