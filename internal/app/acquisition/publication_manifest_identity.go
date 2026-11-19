package acquisition

import (
	"encoding/json"
	"fmt"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The first recorded manifest is evidence, not a mutable hint. Apply the same
// check to import reconstruction and every destructive cleanup pass.
func verifyRecordedPublicationManifest(pub sqlite.Publication, live ports.Manifest) error {
	if pub.ManifestJSON == "" {
		return nil
	}
	var recorded ports.Manifest
	if err := json.Unmarshal([]byte(pub.ManifestJSON), &recorded); err != nil {
		return fmt.Errorf("%w: recorded publication manifest is unreadable", errManifestInvalid)
	}
	if len(recorded.Files) > 0 {
		if err := verifyManifestShape(recorded, pub.Generation); err != nil {
			return err
		}
	}
	expected := recorded.Digest
	if expected == "" && len(recorded.Files) > 0 {
		var err error
		expected, err = ports.ManifestDigest(recorded.Files)
		if err != nil {
			return err
		}
	}
	if expected == "" {
		return nil
	}
	actual, err := ports.ManifestDigest(live.Files)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("%w: publication file list changed from its recorded seal", errManifestInvalid)
	}
	return nil
}

func recordedPublicationFileMatches(pub sqlite.Publication, file ports.ManifestFile) bool {
	var recorded ports.Manifest
	if json.Unmarshal([]byte(pub.ManifestJSON), &recorded) != nil || verifyManifestShape(recorded, pub.Generation) != nil {
		return false
	}
	for _, before := range recorded.Files {
		if before.Path == file.Path && before.Bytes == file.Bytes && len(before.SHA256) == 64 && before.SHA256 == file.SHA256 {
			return true
		}
	}
	return false
}
