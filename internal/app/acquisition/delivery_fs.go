package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Rooted, symlink-refusing file access shared by the publication cleanup,
// the replacement worker and the operator's storage actions.

// openDeliveryDirectoryNoSymlinks walks each directory entry with Lstat
// before opening it. os.Root confines traversal but intentionally follows
// in-root symlinks, so cleanup and accepted-manifest reads reject those too.
func openDeliveryDirectoryNoSymlinks(base *os.Root, rel string) (*os.Root, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("directory escapes verified root")
	}
	current, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return current, nil
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			_ = current.Close()
			return nil, fmt.Errorf("unsafe directory component")
		}
		info, err := current.Lstat(part)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = current.Close()
			return nil, fmt.Errorf("symlink or non-directory in accepted path")
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openDeliveryFileNoSymlinks(base *os.Root, rel string) (*os.Root, *os.File, os.FileInfo, error) {
	clean := filepath.Clean(rel)
	parentRel, name := filepath.Dir(clean), filepath.Base(clean)
	parent, err := openDeliveryDirectoryNoSymlinks(base, parentRel)
	if err != nil {
		return nil, nil, nil, err
	}
	named, err := parent.Lstat(name)
	if err != nil {
		_ = parent.Close()
		return nil, nil, nil, err
	}
	if named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() {
		_ = parent.Close()
		return nil, nil, nil, fmt.Errorf("accepted manifest path is not a regular non-symlink file")
	}
	file, err := parent.Open(name)
	if err != nil {
		_ = parent.Close()
		return nil, nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(named, opened) {
		_ = file.Close()
		_ = parent.Close()
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("accepted manifest file changed while opening")
	}
	return parent, file, named, nil
}

func digestOpenFile(ctx context.Context, file *os.File) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var total int64
	src := newVerifyReader(ctx, file)
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", total, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func syncRootDirectory(root *os.Root, path string) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
