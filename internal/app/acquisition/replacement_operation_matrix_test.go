package acquisition

import (
	"errors"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A reviewed replacement operation's worker step by operation state and by
// what its own downloads are doing (contract section 3.3, item 3: the
// operation owns its next grab and advances with retained bytes).
func TestReplacementOperationStepMatrix(t *testing.T) {
	cases := []struct {
		name      string
		releases  []ports.Release
		state     string
		episodes  func(f *v2Fixture) []int64
		prepare   func(f *v2Fixture, op sqlite.ReplacementOperation)
		indexer   ports.Indexer
		wantState string
		wantError string
		wantAdds  int
	}{
		{name: "an episode that left the item fails the operation", state: "searching",
			episodes:  func(*v2Fixture) []int64 { return []int64{999999} },
			wantState: "failed", wantError: "no longer belongs to this item"},
		{name: "its own active download keeps it replacing", state: "searching",
			prepare: func(f *v2Fixture, op sqlite.ReplacementOperation) {
				dl := f.grab(1, epOne)
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET replacement_operation_id=? WHERE id=?`, op.ID, dl.ID); err != nil {
					f.t.Fatal(err)
				}
			},
			wantState: "replacing", wantError: "still active", wantAdds: 1},
		{name: "a failed attempt does not wait for custody", state: "replacing",
			releases: []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-NEXT", 10)},
			prepare: func(f *v2Fixture, op sqlite.ReplacementOperation) {
				f.deferGrabs()
				dl := f.grab(1, epOne)
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET replacement_operation_id=? WHERE id=?`, op.ID, dl.ID); err != nil {
					f.t.Fatal(err)
				}
				f.observe(f.row(dl.ID), terminalFact(f.row(dl.ID), failed(ports.ReasonHealth, true), "event 1"))
			},
			wantState: "replacing", wantAdds: 2},
		{name: "no acceptable release waits for one", state: "searching",
			wantState: "waiting_for_release", wantError: "no acceptable replacement release"},
		{name: "a failing indexer says the search was partial", state: "searching",
			indexer:   erroringIndexer{errors.New("indexer 503")},
			wantState: "waiting_for_release", wantError: "one or more indexer searches failed"},
		{name: "another acquisition reserving the episode is waited for", state: "searching",
			releases: []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-NEXT", 10)},
			prepare: func(f *v2Fixture, op sqlite.ReplacementOperation) {
				f.grab(1, epOne) // an ordinary acquisition of the same episode
			},
			wantState: "waiting_for_release", wantError: "waiting for another acquisition", wantAdds: 1},
		{name: "a quality replacement only grabs an upgrade", state: "searching",
			releases:  []ports.Release{rel("Test.Show.S01E01.480p.WEB-DL.x264-WORSE", 10)},
			wantState: "waiting_for_release", wantError: "no acceptable replacement release"},
		{name: "the operation's own source release is never grabbed again", state: "searching",
			releases: []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-SOURCE", 10)},
			prepare: func(f *v2Fixture, op sqlite.ReplacementOperation) {
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE replacement_operations SET source_release=?, source_indexer='idx' WHERE operation_id=?`,
					"Test.Show.S01E01.1080p.WEB-DL.x264-SOURCE", op.ID); err != nil {
					f.t.Fatal(err)
				}
			},
			wantState: "waiting_for_release"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, tc.releases)
			if tc.indexer != nil {
				f.svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return tc.indexer }
			}
			e1, err := f.db.GetEpisodeID(f.ctx, f.item, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			episodes := []int64{e1}
			if tc.episodes != nil {
				episodes = tc.episodes(f)
			}
			intent := "replace_quality"
			if strings.Contains(tc.name, "source release") || strings.Contains(tc.name, "custody") {
				intent = "replace_bad"
			}
			op, _, err := f.db.CreateReplacementOperation(f.ctx, sqlite.ReplacementOperation{ID: "op-matrix", RequestID: "op-matrix", Fingerprint: "op-matrix",
				State: tc.state, Intent: intent, Scope: "episodes", MediaItemID: f.item, EpisodeIDs: episodes,
				ReasonCode: "quality_preference", Reason: "matrix"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.prepare != nil {
				tc.prepare(f, op)
			}
			if err := f.svc.processReplacementOperation(f.ctx, op); err != nil && tc.indexer == nil {
				t.Fatal(err)
			}
			got, err := f.db.GetReplacementOperation(f.ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.wantState || !strings.Contains(got.LastError, tc.wantError) {
				t.Fatalf("operation = %s %q, want %s %q", got.State, got.LastError, tc.wantState, tc.wantError)
			}
			if tc.wantAdds > 0 && f.runner.addCount() != tc.wantAdds {
				t.Fatalf("adds = %d, want %d", f.runner.addCount(), tc.wantAdds)
			}
			if tc.wantAdds == 0 && f.runner.addCount() != 0 {
				t.Fatalf("adds = %d, want no grab", f.runner.addCount())
			}
		})
	}
}
