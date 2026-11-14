package household

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type Review struct {
	RequestID   int64     `json:"requestId"`
	Revision    int64     `json:"revision"`
	Title       string    `json:"title"`
	Scope       string    `json:"scope"`
	Destination string    `json:"destination"`
	RootID      int64     `json:"rootId"`
	ProfileID   int64     `json:"profileId"`
	ProfileName string    `json:"profileName"`
	Present     int       `json:"present"`
	Missing     int       `json:"missing"`
	Offloaded   int       `json:"offloaded"`
	CheckedAt   time.Time `json:"checkedAt"`
	Fingerprint string    `json:"fingerprint"`
	NewTitle    bool      `json:"newTitle"`
	plan        sqlite.HouseholdApproval
}

// Review recomputes current facts without adding a title or changing monitoring.
func (s *Service) Review(ctx context.Context, id, accountID, rootID, profileID int64) (Review, error) {
	out := Review{}
	request, err := s.DB.HouseholdRequest(ctx, id, accountID, true)
	if err != nil {
		return out, err
	}
	if request.State != "pending" && request.State != "needs_review" {
		return out, sqlite.ErrRequestChanged
	}
	item, err := s.providerTitle(ctx, request.Kind, request.TMDBID)
	if err != nil {
		return out, err
	}
	if domain.ExternalIDsConflict(item.IDs, request.IDs) {
		return out, fmt.Errorf("request identity changed; review the catalogue record")
	}
	if item.Kind == domain.KindMovie && !knownAired(item.ReleaseDate, time.Now()) {
		return out, fmt.Errorf("a verified release date is needed before acquisition")
	}
	facts, err := s.DB.LookupAvailability(ctx, []domain.AvailabilityTarget{{Kind: item.Kind, IDs: item.IDs}})
	if err != nil {
		return out, err
	}
	if len(facts) != 1 || facts[0].Catalog == "ambiguous" || facts[0].Catalog == "unknown" {
		return out, fmt.Errorf("library identity could not be established")
	}
	plan := sqlite.HouseholdApproval{RequestID: id, RequestRevision: request.Revision}
	if facts[0].Catalog == "present" {
		item, err = s.DB.GetMediaItemFull(ctx, facts[0].LibraryItemID)
		if err != nil {
			return out, err
		}
		if domain.ExternalIDsConflict(item.IDs, request.IDs) {
			return out, sqlite.ErrRequestChanged
		}
		if err = s.Library.VerifyPrimaryPresence(ctx, item.ID); err != nil {
			return out, err
		}
		evidence, e := s.DB.HouseholdEvidence(ctx, item.ID)
		if e != nil {
			return out, e
		}
		plan.Item = item
		plan.Evidence = evidence
		plan.Policy = domain.HouseholdPolicy{ItemID: item.ID, Kind: item.Kind, TMDBID: item.IDs.TMDB, IDs: item.IDs, RootID: evidence.RootID, ProfileID: item.QualityProfileID, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, Path: evidence.Path}
	} else {
		if rootID <= 0 {
			return out, fmt.Errorf("choose a library destination for the new title")
		}
		destination, e := s.Library.PlanHouseholdAdd(ctx, item, rootID, profileID)
		if e != nil {
			return out, e
		}
		plan.New = true
		plan.Item = destination.Item
		plan.LibraryRevision = destination.LibraryRevision
		plan.Policy = domain.HouseholdPolicy{Kind: item.Kind, TMDBID: item.IDs.TMDB, IDs: item.IDs, RootID: destination.Item.RootFolderID, ProfileID: destination.Item.QualityProfileID, RootGeneration: destination.RootGeneration, SharedRevision: destination.SharedRevision, RootPath: destination.RootPath, RootIdentity: destination.RootIdentity, Path: destination.Item.Path}
	}
	item = plan.Item
	if item.Kind == domain.KindMovie {
		if !plan.New && plan.Evidence.PresentMovie {
			out.Present = 1
		} else {
			out.Missing = 1
			plan.Missing = []domain.RequestEpisode{{}}
			if item.Offloaded() {
				out.Offloaded = 1
			}
		}
		out.Scope = "Movie · primary copy"
	} else {
		byCoordinate := map[domain.RequestEpisode]domain.Episode{}
		for _, season := range item.Seasons {
			for _, ep := range season.Episodes {
				byCoordinate[domain.RequestEpisode{Season: ep.SeasonNumber, Episode: ep.EpisodeNumber}] = ep
			}
		}
		if len(request.Scope.Episodes) == 0 {
			return out, fmt.Errorf("request has no verified aired episode scope")
		}
		for _, coordinate := range request.Scope.Episodes {
			ep, exists := byCoordinate[coordinate]
			if !exists || coordinate.Season <= 0 || !knownAired(ep.AirDate, time.Now()) {
				return out, fmt.Errorf("requested episode inventory changed")
			}
			if !plan.New && plan.Evidence.PresentEpisodes[ep.ID] {
				out.Present++
			} else {
				out.Missing++
				plan.Missing = append(plan.Missing, coordinate)
				if ep.Offloaded() {
					out.Offloaded++
				}
			}
		}
		out.Scope = fmt.Sprintf("%d frozen aired regular episodes · primary copy", len(request.Scope.Episodes))
	}
	operations, err := s.DB.PendingOffloadOperationsForItem(ctx, item.ID)
	if err != nil {
		return out, err
	}
	for _, op := range operations {
		plan.Policy.OffloadOperations = append(plan.Policy.OffloadOperations, op.ID)
	}
	slices.Sort(plan.Policy.OffloadOperations)
	profile, err := s.DB.GetProfile(ctx, plan.Policy.ProfileID)
	if err != nil {
		return out, err
	}
	plan.Policy.ProfileFingerprint, err = s.DB.HouseholdProfileFingerprint(ctx, plan.Policy.ProfileID)
	if err != nil {
		return out, err
	}
	out.RequestID = id
	out.Revision = request.Revision
	out.Title = request.Title
	out.Destination = plan.Policy.Path
	out.RootID = plan.Policy.RootID
	out.ProfileID = plan.Policy.ProfileID
	out.ProfileName = profile.Name
	out.NewTitle = plan.New
	// Only semantic review facts enter the confirmation fingerprint. Performing a
	// second check updates its timestamp, not the meaning of the reviewed scope.
	data, err := json.Marshal(struct {
		Request, Revision, LibraryRevision, ItemRevision int64
		Policy                                           domain.HouseholdPolicy
		Evidence                                         string
		Present, Missing, Offloaded                      int
	}{id, request.Revision, plan.LibraryRevision, plan.Evidence.ItemRevision, plan.Policy, plan.Evidence.Fingerprint, out.Present, out.Missing, out.Offloaded})
	if err != nil {
		return out, err
	}
	hash := sha256.Sum256(data)
	out.Fingerprint = hex.EncodeToString(hash[:])
	out.CheckedAt = time.Now()
	out.plan = plan
	return out, nil
}
func (s *Service) Approve(ctx context.Context, a domain.Account, digest string, id, rootID, profileID int64, fingerprint, note string, reacquire bool) error {
	review, err := s.Review(ctx, id, a.ID, rootID, profileID)
	if err != nil {
		return err
	}
	if review.Fingerprint != fingerprint {
		return sqlite.ErrRequestChanged
	}
	if review.Offloaded > 0 && !reacquire {
		return fmt.Errorf("explicitly include the offloaded episodes before approving")
	}
	review.plan.Policy.Reacquire = reacquire
	review.plan.PrivateNote = note
	return s.DB.ApproveHouseholdRequest(ctx, a, digest, review.plan)
}
