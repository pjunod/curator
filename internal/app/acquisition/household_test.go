package acquisition

import (
	"context"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHouseholdImportRejectsExtraEpisodeAndArrivingPrimary(t *testing.T) {
	s, db, id := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	root, err := db.AddRootFolderWithPresence(ctx, filepath.Dir(item.Path), domain.RootKind("series"), "volume")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_items SET root_folder_id=? WHERE id=?`, root.ID, id); err != nil {
		t.Fatal(err)
	}
	item, err = db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	identity, generation, err := db.PresenceRootBinding(ctx, root.ID, root.Path)
	if err != nil {
		t.Fatal(err)
	}
	ep1, err := db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ep2, err := db.GetEpisodeID(ctx, id, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.SetHouseholdVerifier(func(context.Context, int64) error { return nil })
	hash, e := db.HouseholdProfileFingerprint(ctx, item.QualityProfileID)
	if e != nil {
		t.Fatal(e)
	}
	origin := &domain.HouseholdOrigin{Season: 1, Episode: 1, Policy: domain.HouseholdPolicy{Kind: item.Kind, IDs: item.IDs, RootID: root.ID, ProfileID: item.QualityProfileID, ProfileFingerprint: hash}, ItemID: id, Kind: item.Kind, TMDBID: item.IDs.TMDB, EpisodeIDs: []int64{ep1}, Path: item.Path, RootID: root.ID, RootPath: root.Path, RootIdentity: identity, RootGeneration: generation}
	scope := importScope{Household: origin, CopyID: 0, SourceGeneration: "generation", Transfer: "transfer"}
	placement := sqlite.Placement{ItemID: id, Target: filepath.Join(item.Path, "new.mkv"), EpisodeIDs: []int64{ep1, ep2}, SHA256: "new"}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("mixed episode file exceeded approval")
	}
	placement.EpisodeIDs = []int64{ep2}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("extra episode admitted")
	}
	placement.EpisodeIDs = []int64{ep1}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err != nil {
		t.Fatal("exact empty primary scope refused", err)
	}
	arrived := filepath.Join(item.Path, "arrived.mkv")
	if err = os.WriteFile(arrived, []byte("keep arrived primary"), 0600); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, id, 0, arrived, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1}); err != nil {
		t.Fatal(err)
	}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("household request implicitly upgraded arriving primary")
	}
	bytes, err := os.ReadFile(arrived)
	if err != nil || string(bytes) != "keep arrived primary" {
		t.Fatal("arrival changed", err)
	}
	origin.RootGeneration++
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("changed volume accepted")
	}
}

func TestHouseholdOffloadNarrowingIsCumulativeAndPrimaryOnly(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	show := mustItem(t, f.db, f.show)
	copyID, err := f.db.AddMediaCopy(ctx, domain.MediaCopy{MediaItemID: show.ID, QualityProfileID: 1, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	primary := map[int]string{}
	additional := map[int]string{}
	for _, ep := range show.Seasons[0].Episodes {
		primary[ep.EpisodeNumber] = filepath.Join(show.Path, fmt.Sprintf("S01E%02d-primary.mkv", ep.EpisodeNumber))
		additional[ep.EpisodeNumber] = filepath.Join(show.Path, fmt.Sprintf("S01E%02d-additional.mkv", ep.EpisodeNumber))
		f.file(t, show.ID, primary[ep.EpisodeNumber], "primary", "", ep.ID)
		fid := f.file(t, show.ID, additional[ep.EpisodeNumber], "additional", "", ep.ID)
		if _, err = f.db.W.ExecContext(ctx, `UPDATE media_files SET copy_id=? WHERE id=?`, copyID, fid); err != nil {
			t.Fatal(err)
		}
	}
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	op := f.start(t, req, f.preview(t, req))
	f.wait(t, op.ID)
	if err = f.svc.CancelHouseholdOffloadDeliveries(ctx, show.ID, domain.RequestScope{Episodes: []domain.RequestEpisode{{Season: 1, Episode: 1}}}, nil); err != nil {
		t.Fatal(err)
	}
	before, err := f.db.OperationDeliveries(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range before {
		if delivery.Status == "pending" && !strings.Contains(delivery.Payload, filepath.Base(primary[1])) {
			t.Fatal("empty operation list changed another approval")
		}
	}
	for _, episode := range []int{1, 2, 1} {
		if err = f.svc.CancelHouseholdOffloadDeliveries(ctx, show.ID, domain.RequestScope{Episodes: []domain.RequestEpisode{{Season: 1, Episode: episode}}}, []string{op.ID}); err != nil {
			t.Fatal(err)
		}
	}
	deliveries, err := f.db.OperationDeliveries(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, delivery := range deliveries {
		if delivery.Status != "pending" {
			continue
		}
		pending++
		for _, path := range primary {
			if strings.Contains(delivery.Payload, filepath.Base(path)) {
				t.Fatal("primary path resurrected after sibling retry", delivery.Payload)
			}
		}
		for _, path := range additional {
			if !strings.Contains(delivery.Payload, filepath.Base(path)) {
				t.Fatal("additional-copy offload was cancelled", delivery.Payload)
			}
		}
	}
	if pending == 0 {
		t.Fatal("additional copy notification disappeared")
	}
	current := mustItem(t, f.db, show.ID)
	for _, ep := range current.Seasons[0].Episodes {
		if !ep.Offloaded() {
			t.Fatal("request re-enabled ordinary episode acquisition")
		}
	}
	for _, id := range wantedIDs(t, f.svc) {
		if strings.HasPrefix(id, fmt.Sprintf("episode:%d:", show.ID)) {
			t.Fatal("household reacquire made monitored copies wanted", id)
		}
	}
}
func TestHouseholdMovieOffloadKeepsMonitoredCopiesQuiet(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	movie, primary, _ := f.movieWithFiles(t)
	copyID, err := f.db.AddMediaCopy(ctx, domain.MediaCopy{MediaItemID: movie.ID, QualityProfileID: 1, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(movie.Path, "additional.mkv")
	fid := f.file(t, movie.ID, extra, "additional", "")
	if _, err = f.db.W.ExecContext(ctx, `UPDATE media_files SET copy_id=? WHERE id=?`, copyID, fid); err != nil {
		t.Fatal(err)
	}
	req := OffloadRequest{ItemID: movie.ID, Scope: "item"}
	op := f.start(t, req, f.preview(t, req))
	f.wait(t, op.ID)
	if err = f.svc.CancelHouseholdOffloadDeliveries(ctx, movie.ID, domain.RequestScope{}, []string{op.ID}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := f.db.OperationDeliveries(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if delivery.Status == "pending" && (strings.Contains(delivery.Payload, filepath.Base(primary)) || !strings.Contains(delivery.Payload, filepath.Base(extra))) {
			t.Fatal("movie notification exceeded primary scope", delivery.Payload)
		}
	}
	if !mustItem(t, f.db, movie.ID).Offloaded() {
		t.Fatal("movie offload epoch cleared before primary bytes")
	}
	for _, id := range wantedIDs(t, f.svc) {
		if id == fmt.Sprintf("movie:%d", movie.ID) || strings.HasPrefix(id, fmt.Sprintf("movie:%d:", movie.ID)) {
			t.Fatal("monitored copy made wanted", id)
		}
	}
}

func TestFailedMonitoredHouseholdEpisodeNeverFallsBackToSeasonPlanning(t *testing.T) {
	client := &fakeClient{}
	s, db, id := setup(t, nil, client)
	ctx := context.Background()
	want := fmt.Sprintf("episode:%d:1:1", id)
	downloadID, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: id, ClientID: 1, Protocol: "torrent", State: "grabbed", Intent: "automatic", Transfer: "household-failed", WantableIDs: []string{want}})
	if err != nil {
		t.Fatal(err)
	}
	download, err := db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO household_download_origins(download_id,generation,target_id,approval_generation,scope_json,policy_json) VALUES(?,?,1,1,'{}','{}')`, downloadID, download.GenerationKey); err != nil {
		t.Fatal(err)
	}
	won, err := s.recordTerminalWith(ctx, download, ports.ClientConfig{ID: 1, Type: "qbittorrent"}, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: "payload", BlamesRelease: true, Salvage: "none"}, "household regression", 1, false, 0)
	if err != nil || !won {
		t.Fatal("failure", won, err)
	}
	var jobs int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_grabs WHERE download_id=?`, downloadID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("failure enqueued ordinary replacement", jobs, err)
	}
	// Simulate a legacy queued job to exercise the worker's independent fence.
	if _, err = db.W.ExecContext(ctx, `DROP TRIGGER household_no_generic_replacement`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO replacement_grabs(download_id,wantable_ids,created_at,next_at) VALUES(?,?,0,0)`, downloadID, fmt.Sprintf(`["%s"]`, want)); err != nil {
		t.Fatal(err)
	}
	s.RunReplacementGrabs(ctx)
	var plans int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM acquisition_plans`).Scan(&plans); err != nil || plans != 0 {
		t.Fatal("household failure reached season planner", plans, err)
	}
	if len(client.added) != 0 {
		t.Fatal("household failure submitted another download", client.added)
	}
}
