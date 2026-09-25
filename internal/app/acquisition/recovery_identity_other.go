//go:build !darwin && !linux && !freebsd

package acquisition

import "os"

func recoveryDirectoryIdentity(info os.FileInfo) any {
	return []any{info.Name(), info.Mode().String(), info.ModTime().UnixNano()}
}
