package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// namedPlacementStage binds the existing filesystem seam to this journaled
// object. Fault/blocked-I/O tests exercise the same seam as ordinary staging.
// The value is scoped to a single synchronous placeFile invocation.
type namedPlacementStageKey struct{}
type namedPlacementStage struct {
	source, target string
	write          func(context.Context, func(int64, int64)) error
}

// stageImportPlacement writes directly into the name recorded before creation.
// The open object's identity is persisted before any payload bytes are copied.
// On one filesystem the stage is a hard link to the source and nothing is
// copied; otherwise the bytes are hashed as they are written, so the copy is
// proven against the recorded digest without reading anything back. A stage a
// failed attempt left behind is scratch: the next attempt of this placement
// discards it and writes it again (preparePlacementPublication).
func (s *Service) stageImportPlacement(ctx context.Context, p *sqlite.Placement, progress func(int64, int64)) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if linked, err := s.linkImportStage(ctx, p); err != nil || linked {
		if err == nil && progress != nil {
			progress(p.Size, p.Size)
		}
		return err
	}
	in, err := os.Open(p.Source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, in.Close()) }()
	out, err := os.OpenFile(p.Temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode())
	if err != nil {
		return err
	}
	// Close may block in NFS writeback. The owning import ticket/admission token
	// stays live until this function and its close have actually returned.
	closeOut := func(e error) error { return errors.Join(e, out.Close()) }
	if err = out.Chmod(fileMode()); err != nil {
		return closeOut(err)
	}
	info, err := out.Stat()
	if err != nil {
		return closeOut(err)
	}
	p.TemporaryObjectIdentity = deliveryFileStableIdentity(info)
	if p.TemporaryObjectIdentity == "" {
		return closeOut(fmt.Errorf("stage object identity unavailable"))
	}
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err = s.db.UpdatePreparedPlacement(local, *p)
	cancel()
	if err != nil {
		return closeOut(err)
	}
	if err = syncPublicationDirectory(p.Temporary); err != nil {
		return closeOut(err)
	}
	var n int64
	digest := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		if err = ctx.Err(); err != nil {
			return closeOut(err)
		}
		read, e := in.Read(buffer)
		if read > 0 {
			written, w := out.Write(buffer[:read])
			n += int64(written)
			digest.Write(buffer[:written])
			if progress != nil {
				progress(n, p.Size)
			}
			if w != nil {
				return closeOut(w)
			}
			if written != read {
				return closeOut(fmt.Errorf("short stage write"))
			}
		}
		if e != nil {
			if !errors.Is(e, io.EOF) {
				return closeOut(e)
			}
			break
		}
	}
	if n != p.Size {
		return closeOut(fmt.Errorf("source size changed during staging"))
	}
	if err = out.Sync(); err != nil {
		return closeOut(err)
	}
	if err = closeOut(nil); err != nil {
		return err
	}
	recordStageDigest(ctx, hex.EncodeToString(digest.Sum(nil)))
	return nil
}

// linkImportStage makes the stage a hard link to the source when both are on
// one filesystem: placement is then a link and a rename, with no bytes read
// or written. A link that the filesystem refuses (another volume, a mount
// that does not support it) is not an error: the caller copies. A recovery
// import always copies (its sources are an operator's selection, not a
// payload Curator owns).
func (s *Service) linkImportStage(ctx context.Context, p *sqlite.Placement) (bool, error) {
	if _, recovery := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); recovery {
		return false, nil
	}
	if err := os.Link(p.Source, p.Temporary); err != nil {
		return false, nil
	}
	info, err := os.Lstat(p.Temporary)
	if err != nil {
		return false, err
	}
	src, err := os.Lstat(p.Source)
	if err != nil || !os.SameFile(src, info) || !info.Mode().IsRegular() {
		_ = discardStage(p.Temporary)
		return false, fmt.Errorf("linked stage does not name the source object: %s", p.Source)
	}
	if info.Size() != p.Size {
		// As for a copy: the stage stays as scratch for the next attempt.
		return false, fmt.Errorf("source size changed during staging")
	}
	if err = applyMode(p.Temporary, fileMode()); err != nil {
		_ = discardStage(p.Temporary)
		return false, err
	}
	p.TemporaryObjectIdentity = deliveryFileStableIdentity(info)
	if p.TemporaryObjectIdentity == "" {
		_ = discardStage(p.Temporary)
		return false, fmt.Errorf("stage object identity unavailable")
	}
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err = s.db.UpdatePreparedPlacement(local, *p)
	cancel()
	if err != nil {
		return false, err
	}
	if err = syncPublicationDirectory(p.Temporary); err != nil {
		return false, err
	}
	recordStageLinked(ctx)
	return true, nil
}
