package library

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
)

type HouseholdAddPlan struct {
	Item                                            domain.MediaItem
	LibraryRevision, SharedRevision, RootGeneration int64
	RootPath, RootIdentity                          string
}

// PlanHouseholdAdd is read-only. It refuses existing unindexed media instead of
// interpreting it as permission to acquire another copy or adopting it silently.
func (s *Service) PlanHouseholdAdd(ctx context.Context, item domain.MediaItem, rootID, profileID int64) (HouseholdAddPlan, error) {
	out := HouseholdAddPlan{}
	revision, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return out, err
	}
	root, err := s.db.GetRootFolder(ctx, rootID)
	if err != nil || !root.Kind.Accepts(item.Kind) {
		return out, fmt.Errorf("choose a compatible library destination")
	}
	if profileID == 0 {
		profileID = s.db.DefaultProfileID(ctx, item.Kind)
	}
	if err = s.validateItemProfile(ctx, item.Kind, profileID, ""); err != nil {
		return out, err
	}
	item.RootFolderID = root.ID
	item.QualityProfileID = profileID
	item.ID = 0
	item.Path, err = s.freeFolder(ctx, root.Path, item)
	if err != nil {
		return out, err
	}
	before, identity, err := s.checkPresenceRoot(ctx, root.ID, root.Path)
	if err != nil {
		return out, err
	}
	generation, err := s.db.PresenceRootGeneration(ctx, root.ID, root.Path, identity)
	if err != nil || generation == 0 {
		return out, fmt.Errorf("verify this library volume before approving requests")
	}
	if err = validatePresenceScope(root.Path, item.Path); err != nil {
		return out, err
	}
	info, err := os.Lstat(item.Path)
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	if err == nil {
		if !info.IsDir() {
			return out, fmt.Errorf("destination is not a directly walkable folder")
		}
		if err = filepath.WalkDir(item.Path, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("destination contains a symlink")
			}
			if !entry.IsDir() && filename.IsVideo(path) {
				return fmt.Errorf("destination contains unindexed media; scan or assign it before approval")
			}
			return nil
		}); err != nil {
			return out, err
		}
	}
	after, err := os.Stat(root.Path)
	if err != nil || !os.SameFile(before, after) {
		return out, fmt.Errorf("library volume changed")
	}
	_, shared, err := s.db.PresenceRevision(ctx, 0)
	if err != nil {
		return out, err
	}
	item.Monitored = false
	item.Monitor = "none"
	item.SortTitle = domain.SortTitle(item.Title)
	item.Source = "tmdb"
	for i := range item.Seasons {
		item.Seasons[i].Monitored = false
		for j := range item.Seasons[i].Episodes {
			item.Seasons[i].Episodes[j].Monitored = false
		}
	}
	return HouseholdAddPlan{Item: item, LibraryRevision: revision, SharedRevision: shared, RootGeneration: generation, RootPath: root.Path, RootIdentity: identity}, nil
}
func (s *Service) VerifyPrimaryPresence(ctx context.Context, id int64) error {
	return s.verifyPresence(ctx, id, true)
}
