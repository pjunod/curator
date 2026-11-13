package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type VolumeReview struct {
	State            sqlite.PresenceRootState `json:"state"`
	ObservedIdentity string                   `json:"observedIdentity"`
	Empty            bool                     `json:"empty"`
	Blocker          string                   `json:"blocker,omitempty"`
	Fingerprint      string                   `json:"fingerprint"`
}

func (s *Service) PreviewLibraryVolume(ctx context.Context, id int64) (VolumeReview, error) {
	state, err := s.db.PresenceRootState(ctx, id)
	if err != nil {
		return VolumeReview{}, err
	}
	v := VolumeReview{State: state}
	info, err := os.Stat(state.Path)
	switch {
	case err != nil || !info.IsDir():
		v.Blocker = "The library volume is unavailable. Restore its mount before confirming."
	default:
		f, e := os.Open(state.Path)
		if e != nil {
			v.Blocker = "The library volume cannot be read."
		} else {
			names, e := f.Readdirnames(1)
			_ = f.Close()
			if e != nil && !errors.Is(e, io.EOF) {
				v.Blocker = "The library volume cannot be read."
			}
			v.Empty = len(names) == 0
		}
		v.ObservedIdentity = presenceRootIdentity(info)
		if v.ObservedIdentity == "" {
			v.Blocker = "Volume identity verification is unsupported on this platform."
		}
	}
	if state.ActiveWork > 0 {
		v.Blocker = "Active library work still owns this volume. Wait for it to finish or stop safely before reviewing again."
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	hash := sha256.Sum256(data)
	v.Fingerprint = hex.EncodeToString(hash[:])
	return v, nil
}
func (s *Service) ConfirmLibraryVolume(ctx context.Context, id int64, fingerprint string, ack bool) error {
	if !ack {
		return fmt.Errorf("review and acknowledge this volume before confirming")
	}
	current, err := s.PreviewLibraryVolume(ctx, id)
	if err != nil {
		return err
	}
	if current.Fingerprint != fingerprint {
		return fmt.Errorf("volume or library custody changed; review it again")
	}
	if current.Blocker != "" {
		return fmt.Errorf("%s", current.Blocker)
	}
	return s.db.BindPresenceRoot(ctx, current.State, current.ObservedIdentity)
}
