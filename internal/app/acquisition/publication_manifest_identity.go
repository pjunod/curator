package acquisition

import (
	"encoding/json"
	"fmt"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The first recorded manifest is evidence, not a mutable hint. Apply the same
// check to import reconstruction and every destructive cleanup pass.
func verifiedRecordedPublicationManifest(pub sqlite.Publication, live ports.Manifest, restoreMissingDigests bool) (ports.Manifest, error) {
	if err := verifyManifestShape(live, pub.Generation); err != nil {
		return live, fmt.Errorf("%w: %v", errManifestInvalid, err)
	}
	if pub.ManifestJSON == "" {
		return live, nil
	}
	var recorded ports.Manifest
	if err := json.Unmarshal([]byte(pub.ManifestJSON), &recorded); err != nil {
		return live, fmt.Errorf("%w: recorded publication manifest is unreadable", errManifestInvalid)
	}
	if len(recorded.Files) > 0 {
		if err := verifyManifestShape(recorded, pub.Generation); err != nil {
			return live, fmt.Errorf("%w: %v", errManifestInvalid, err)
		}
	}
	expected := recorded.Digest
	if expected == "" && len(recorded.Files) > 0 {
		var err error
		expected, err = ports.ManifestDigest(recorded.Files)
		if err != nil {
			return live, err
		}
	}
	if expected == "" {
		return live, nil
	}
	// Cleanup can use an original recorded hash when an older writer omits
	// it. Import stays strict: its same-volume placement fast path does not
	// promise a fresh hash check, so omitted evidence cannot authorize import.
	if restoreMissingDigests {
		live.Files = append([]ports.ManifestFile(nil), live.Files...)
		for i, file := range live.Files {
			if file.SHA256 != "" {
				continue
			}
			for _, before := range recorded.Files {
				if before.Path == file.Path && before.Bytes == file.Bytes {
					live.Files[i].SHA256 = before.SHA256
					break
				}
			}
		}
	}
	actual, err := ports.ManifestDigest(live.Files)
	if err != nil {
		return live, err
	}
	if actual != expected {
		return live, fmt.Errorf("%w: publication file list changed from its recorded seal", errManifestInvalid)
	}
	live.Digest = actual
	return live, nil
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
