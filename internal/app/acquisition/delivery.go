package acquisition

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
)

func deliveryImportablePath(kind domain.MediaKind, name string) bool {
	if kind == domain.KindBook {
		return filename.IsBook(name)
	}
	return filename.IsVideo(name)
}

func safeDeliveryRelativePath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func deliveryRootAndRelative(payload string, roots []string) (string, string, error) {
	// Mapped paths can use a host alias such as /var/folders while the
	// configured root was canonicalized through /private/var. Resolve the
	// existing path's aliases before the lexical containment check; otherwise
	// valid local Runner publications fail closed even though both paths name
	// the same directory tree.
	payload = canonicalCompletedPath(filepath.Clean(payload))
	for _, root := range roots {
		root = canonicalCompletedPath(filepath.Clean(root))
		if !within(root, payload) || root == payload {
			continue
		}
		rel, err := filepath.Rel(root, payload)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return root, rel, nil
		}
	}
	return "", "", fmt.Errorf("mapped Runner publication is outside configured completed storage")
}
