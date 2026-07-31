package acquisition

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
)

// An import that runs out of disk is not a file that "did not qualify".
//
// Field report 2026-07-31: two complete season packs sat in the download
// client's completed folder while Monarr listed exactly the episodes those
// packs contained as missing. Both had imported their first half and
// stopped on a full volume. Nothing surfaced: the per-file loop recorded a
// reason on each failed copy, carried on, and — because SOME files had
// landed — reported the import a success, cleaned up, and moved on. The
// item went back to wanted, the next backlog pass grabbed the same pack
// again, and the loop paid for it in terabytes.
//
// So an out-of-space error is treated as what it is: a condition of the
// machine, not a verdict on the file. It stops the import at the first
// occurrence (every subsequent file would fail the same way), the download
// is surfaced as failed rather than silently half-done, and the retry sweep
// picks it up once there is room.

// retryableIO reports whether an error is an environmental I/O failure —
// something about the disk, not about the release. These are the errors
// worth trying again unchanged; everything else (a quality decision, an
// unparseable name, a cut-short payload) will fail identically forever.
func retryableIO(err error) bool {
	if err == nil {
		return false
	}
	if outOfSpace(err) {
		return true
	}
	for _, e := range []error{syscall.EIO, syscall.EROFS, syscall.ENOTCONN, syscall.ESTALE} {
		if errors.Is(err, e) {
			return true
		}
	}
	var perr *fs.PathError
	if errors.As(err, &perr) && errors.Is(perr, fs.ErrPermission) {
		// A mount that came back read-only, or a container that lost its
		// bind mount: fixable, and the same file will import afterwards.
		return true
	}
	return false
}

// outOfSpace reports whether an error is the volume being full. EDQUOT
// counts: a gluster or NFS mount answers that for the same condition, and
// the quota-backed mount is exactly where this was first seen.
//
// The string fallback is not decoration — an error that crossed a
// subprocess boundary, or one wrapped by a library that dropped the errno,
// arrives as text and means the same thing.
func outOfSpace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left on device") ||
		strings.Contains(msg, "disk quota exceeded") ||
		strings.Contains(msg, "no storage space")
}
