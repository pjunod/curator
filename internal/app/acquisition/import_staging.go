package acquisition

import (
	"context"
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
// Failed/partial stages retain custody; no filename or digest alone permits a
// new process to truncate, delete or resume one on an unqualified NFS client.
func (s *Service) stageImportPlacement(ctx context.Context, p *sqlite.Placement, progress func(int64, int64)) error {
	in, err := os.Open(p.Source)
	if err != nil {
		return err
	}
	defer in.Close()
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
	buffer := make([]byte, 1024*1024)
	for {
		if err = ctx.Err(); err != nil {
			return closeOut(err)
		}
		read, e := in.Read(buffer)
		if read > 0 {
			written, w := out.Write(buffer[:read])
			n += int64(written)
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
	return closeOut(nil)
}
