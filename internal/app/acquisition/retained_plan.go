package acquisition

import (
	"context"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
	"os"
	"path/filepath"
	"strings"
)

// RetainedCandidate names externally evidenced bytes, not an import permission.
// Dry-run plans are observations and cannot authorize mutation or cleanup.
type RetainedCandidate struct {
	Path             string `json:"path"`
	MediaItemID      int64  `json:"media_item_id"`
	CopyID           int64  `json:"copy_id"`
	Season           int    `json:"season"`
	Episodes         []int  `json:"episodes"`
	Evidence         string `json:"evidence"`
	ExpectedSHA256   string `json:"expected_sha256"`
	CompleteCoverage bool   `json:"complete_coverage"`
	Partial          bool   `json:"partial"`
}
type RetainedPlanFile struct {
	RetainedCandidate
	Identity        string   `json:"identity"`
	SHA256          string   `json:"sha256"`
	Bytes           int64    `json:"bytes"`
	Action          string   `json:"action"`
	Destination     string   `json:"destination"`
	ExistingLibrary []string `json:"existing_library"`
	DuplicateOf     string   `json:"duplicate_of,omitempty"`
	CapacityBytes   int64    `json:"capacity_bytes"`
	BlockedReason   string   `json:"blocked_reason"`
}
type RetainedPlan struct {
	LibraryRevision    int64              `json:"library_revision"`
	Provisional        bool               `json:"provisional"`
	MutationAuthorized bool               `json:"mutation_authorized"`
	Files              []RetainedPlanFile `json:"files"`
}

func (s *Service) PlanRetainedRecovery(ctx context.Context, candidates []RetainedCandidate) (RetainedPlan, error) {
	plan := RetainedPlan{Files: []RetainedPlanFile{}}
	if len(candidates) > 1000 {
		return plan, fmt.Errorf("recovery plan file limit exceeded")
	}
	revision, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return plan, err
	}
	plan.LibraryRevision = revision
	active, err := s.db.ListActiveDownloads(ctx)
	if err != nil {
		return plan, err
	}
	placements, err := s.db.PendingPlacements(ctx)
	if err != nil {
		return plan, err
	}
	digests := map[string]string{}
	for _, candidate := range candidates {
		row := RetainedPlanFile{RetainedCandidate: candidate, Action: "hold"}
		if !filepath.IsAbs(candidate.Path) {
			row.BlockedReason = "source path must be absolute"
			plan.Files = append(plan.Files, row)
			continue
		}
		if err = rejectSymlinks(candidate.Path, false); err != nil {
			row.BlockedReason = err.Error()
			plan.Files = append(plan.Files, row)
			continue
		}
		info, e := os.Lstat(candidate.Path)
		if e != nil || !info.Mode().IsRegular() {
			row.BlockedReason = "source is not a regular file"
			plan.Files = append(plan.Files, row)
			continue
		}
		row.Identity = fmt.Sprint(recoveryDirectoryIdentity(info))
		row.SHA256, row.Bytes, e = fileDigest(ctx, candidate.Path)
		if e != nil {
			return plan, e
		}
		if after, e := os.Lstat(candidate.Path); e != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
			return plan, fmt.Errorf("source changed during inventory")
		}
		row.CapacityBytes = row.Bytes + 1024*1024*1024
		if candidate.Partial || strings.HasSuffix(strings.ToLower(candidate.Path), ".part") {
			row.BlockedReason = "known partial; same-job range recovery required"
		}
		if row.BlockedReason == "" && (!candidate.CompleteCoverage || candidate.ExpectedSHA256 == "" || row.SHA256 != candidate.ExpectedSHA256) {
			row.BlockedReason = "complete coverage and authoritative digest are unverified"
		}
		if prior, ok := digests[row.SHA256]; ok {
			row.DuplicateOf = prior
		} else {
			digests[row.SHA256] = candidate.Path
		}
		for _, dl := range active {
			path := dl.ImportPath
			if path == "" {
				path = dl.SavePath
			}
			if path != "" && (within(path, candidate.Path) || within(candidate.Path, path)) {
				plan.Provisional = true
				row.BlockedReason = fmt.Sprintf("active transfer/import %d overlaps source; wait for quiescence", dl.ID)
			}
		}
		for _, placement := range placements {
			if placement.Source == candidate.Path {
				row.BlockedReason = "placement or receipt acknowledgement is pending; source retained"
			}
		}
		item, e := s.db.GetMediaItemFull(ctx, candidate.MediaItemID)
		if e != nil {
			return plan, e
		}
		if item.Kind == "series" && len(candidate.Episodes) == 0 {
			row.BlockedReason = "exact episode mapping required"
		}
		for _, episode := range candidate.Episodes {
			if _, e = s.db.GetEpisodeID(ctx, item.ID, candidate.Season, episode); e != nil && row.BlockedReason == "" {
				row.BlockedReason = "intended episode is not in the library"
			}
		}
		destinationRoot := item.Path
		if candidate.CopyID != 0 {
			copy, err := s.db.GetMediaCopy(ctx, item.ID, candidate.CopyID)
			if err != nil {
				return plan, err
			}
			if copy.Path != "" {
				destinationRoot = copy.Path
			}
		}
		quality := parser.Parse(filepath.Base(candidate.Path)).Quality
		if destinationRoot == "" {
			row.BlockedReason = "library destination is not configured"
		}
		if item.Kind == domain.KindMovie {
			row.Destination = movieDestination(destinationRoot, item, candidate.Path, quality)
		}
		if item.Kind == domain.KindSeries && len(candidate.Episodes) > 0 {
			title := ""
			for _, season := range item.Seasons {
				if season.Number != candidate.Season {
					continue
				}
				for _, episode := range season.Episodes {
					if episode.EpisodeNumber == candidate.Episodes[0] {
						title = episode.Title
					}
				}
			}
			row.Destination = episodeDestination(destinationRoot, item, candidate.Path, quality, candidate.Season, candidate.Episodes, title)
		}
		files, e := s.db.ListFilesForItem(ctx, item.ID)
		if e != nil {
			return plan, e
		}
		for _, file := range files {
			if file.CopyID != candidate.CopyID {
				continue
			}
			digest, _, e := fileDigest(ctx, file.Path)
			if e == nil {
				row.ExistingLibrary = append(row.ExistingLibrary, file.Path+" sha256="+digest)
				if digest == row.SHA256 {
					row.Action = "already_imported"
				}
			}
		}
		if row.BlockedReason == "" && row.Action != "already_imported" {
			row.Action = "preview_through_placement_coordinator"
		}
		if row.BlockedReason != "" {
			row.Action = "hold"
		}
		plan.Files = append(plan.Files, row)
	}
	current, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return plan, err
	}
	if current != revision {
		return plan, fmt.Errorf("library changed during recovery inventory; refresh the plan")
	}
	return plan, nil
}

// ValidateRetainedPlan rechecks observations. Success still grants no lease,
// no source deletion and no authority to bypass the placement coordinator.
func (s *Service) ValidateRetainedPlan(ctx context.Context, plan RetainedPlan) error {
	revision, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return err
	}
	if revision != plan.LibraryRevision {
		return fmt.Errorf("stale recovery plan; another library change committed")
	}
	if plan.Provisional {
		return fmt.Errorf("provisional recovery inventory requires quiescence and a fresh plan")
	}
	active, err := s.db.ListActiveDownloads(ctx)
	if err != nil {
		return err
	}
	pending, err := s.db.PendingPlacements(ctx)
	if err != nil {
		return err
	}
	for _, row := range plan.Files {
		for _, dl := range active {
			path := dl.ImportPath
			if path == "" {
				path = dl.SavePath
			}
			if path != "" && (within(path, row.Path) || within(row.Path, path)) {
				return fmt.Errorf("active work changed; refresh recovery plan")
			}
		}
		for _, receipt := range pending {
			if receipt.Source == row.Path {
				return fmt.Errorf("receipt acknowledgement pending; source retained")
			}
		}
		if row.BlockedReason != "" {
			return fmt.Errorf("recovery file held: %s", row.BlockedReason)
		}
		if err = rejectSymlinks(row.Path, false); err != nil {
			return err
		}
		info, err := os.Lstat(row.Path)
		if err != nil {
			return err
		}
		digest, size, err := fileDigest(ctx, row.Path)
		if err != nil {
			return err
		}
		if fmt.Sprint(recoveryDirectoryIdentity(info)) != row.Identity || digest != row.SHA256 || size != row.Bytes {
			return fmt.Errorf("recovery source identity changed")
		}
	}
	return nil
}
