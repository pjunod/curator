package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
	"slices"
	"strings"
)

func householdIDsMatch(frozen, current domain.ExternalIDs) bool {
	return frozen.TMDB > 0 && frozen.TMDB == current.TMDB && (frozen.TVDB == 0 || frozen.TVDB == current.TVDB) && (frozen.IMDB == "" || strings.EqualFold(frozen.IMDB, current.IMDB))
}
func householdProfileFingerprint(ctx context.Context, q scopeQuerier, id int64) (string, error) {
	var definition string
	var upgrades int64
	if err := q.QueryRowContext(ctx, `SELECT definition,upgrades_allowed FROM quality_profiles WHERE id=?`, id).Scan(&definition, &upgrades); err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal([]byte(definition), &value); err != nil {
		return "", err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", raw, upgrades)))
	return hex.EncodeToString(sum[:]), nil
}
func (d *DB) HouseholdProfileFingerprint(ctx context.Context, id int64) (string, error) {
	return householdProfileFingerprint(ctx, d.R, id)
}
func validateHouseholdOrigin(ctx context.Context, q scopeQuerier, o domain.HouseholdOrigin, p Placement) error {
	policy := o.Policy
	var kind domain.MediaKind
	var ids domain.ExternalIDs
	var root, profile, epoch, generation int64
	var path, rootPath, identity string
	if err := q.QueryRowContext(ctx, `SELECT m.kind,m.tmdb_id,m.tvdb_id,m.imdb_id,m.root_folder_id,m.quality_profile_id,m.path,m.offloaded_at,r.path,p.identity,p.generation FROM media_items m JOIN root_folders r ON r.id=m.root_folder_id JOIN library_presence_roots p ON p.root_id=r.id AND p.root_path=r.path WHERE m.id=?`, o.ItemID).Scan(&kind, &ids.TMDB, &ids.TVDB, &ids.IMDB, &root, &profile, &path, &epoch, &rootPath, &identity, &generation); err != nil {
		return err
	}
	if p.ItemID != o.ItemID || p.CopyID != 0 || kind != o.Kind || kind != policy.Kind || !householdIDsMatch(policy.IDs, ids) || ids.TMDB != o.TMDBID || root != o.RootID || root != policy.RootID || profile != policy.ProfileID || path != o.Path || rootPath != o.RootPath || identity != o.RootIdentity || generation != o.RootGeneration {
		return ErrRequestChanged
	}
	var conflicting bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_items WHERE kind=? AND id<>? AND (tmdb_id=? OR (?<>0 AND tvdb_id=?) OR (?<>'' AND lower(imdb_id)=lower(?))))`, string(kind), o.ItemID, ids.TMDB, ids.TVDB, ids.TVDB, ids.IMDB, ids.IMDB).Scan(&conflicting); err != nil {
		return err
	}
	if conflicting {
		return ErrRequestChanged
	}
	fingerprint, err := householdProfileFingerprint(ctx, q, profile)
	if err != nil {
		return err
	}
	if fingerprint != policy.ProfileFingerprint {
		return ErrRequestChanged
	}
	if kind == domain.KindSeries {
		if len(p.EpisodeIDs) == 0 {
			return ErrRequestChanged
		}
		for _, id := range p.EpisodeIDs {
			if !slices.Contains(o.EpisodeIDs, id) {
				return ErrRequestChanged
			}
			var season, episode int
			if err = q.QueryRowContext(ctx, `SELECT season_number,episode_number,offloaded_at FROM episodes WHERE id=? AND media_item_id=?`, id, o.ItemID).Scan(&season, &episode, &epoch); err != nil {
				return err
			}
			if season != o.Season || episode != o.Episode {
				return ErrRequestChanged
			}
		}
	} else if len(p.EpisodeIDs) > 0 {
		return ErrRequestChanged
	}
	if epoch != o.OffloadEpoch {
		// A redo can settle its own exact committed file after its insert cleared
		// the old epoch. Another transfer or a newer offload can never use that proof.
		var own bool
		if epoch == 0 && o.OffloadEpoch > 0 {
			err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_files f JOIN media_file_placements r ON r.media_file_id=f.id AND r.file_generation=f.file_generation WHERE f.media_item_id=? AND COALESCE(f.copy_id,0)=0 AND f.path=? AND r.source_generation=? AND r.source_transfer=? AND r.sha256=?)`, o.ItemID, p.Target, p.SourceGeneration, p.SourceTransfer, p.SHA256).Scan(&own)
			if err != nil {
				return err
			}
		}
		if !own {
			return ErrRequestChanged
		}
	}
	return nil
}
func (d *DB) CheckHouseholdOrigin(ctx context.Context, o domain.HouseholdOrigin, p Placement) error {
	tx, err := d.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return validateHouseholdOrigin(ctx, tx, o, p)
}
func validateHouseholdPlacementTx(ctx context.Context, tx *sql.Tx, p Placement) error {
	if p.DownloadID == 0 || p.SourceGeneration == "" {
		return nil
	}
	var scope, policy string
	err := tx.QueryRowContext(ctx, `SELECT scope_json,policy_json FROM household_download_origins WHERE download_id=? AND generation=?`, p.DownloadID, p.SourceGeneration).Scan(&scope, &policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var o domain.HouseholdOrigin
	if err = json.Unmarshal([]byte(scope), &o); err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(policy), &o.Policy); err != nil {
		return err
	}
	return validateHouseholdOrigin(ctx, tx, o, p)
}
