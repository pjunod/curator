//go:build unix

package library

import (
	"fmt"
	"os"
	"syscall"
)

func presenceRootIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	return ""
}
