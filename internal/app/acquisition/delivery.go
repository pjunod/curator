package acquisition

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
)

const (
	deliveryPageSize       = 100
	deliveryPagesPerSweep  = 10
	deliveryReceiptsPerRun = 64
	deliveryMaxFiles       = 10000
	deliveryMaxPathBytes   = 4096
	deliveryMaxTotalBytes  = int64(2 << 40) // bound one worker's hash pass to 2 TiB
)

func deliveryAttemptError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func deliveryImportablePath(kind domain.MediaKind, name string) bool {
	if kind == domain.KindBook {
		return filename.IsBook(name)
	}
	return filename.IsVideo(name)
}

type verifiedDeliveryFile struct {
	relative, absolute          string
	root, relativeRoot          string
	size                        int64
	digest                      string
	generation                  string
	rootInfo, payloadInfo, info os.FileInfo
}

func copyHashContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	src = newVerifyReader(ctx, src)
	buf := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
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

func validSHA256(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
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
