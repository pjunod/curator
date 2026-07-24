//go:build unix

package sqlite

import (
	"os"
	"syscall"
)

func statOwner(path string) ownership {
	fi, err := os.Stat(path)
	if err != nil {
		return ownership{}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ownership{}
	}
	return ownership{known: true, uid: int(st.Uid), gid: int(st.Gid), mode: fi.Mode()}
}

func procOwner() ownership {
	return ownership{known: true, uid: os.Getuid(), gid: os.Getgid()}
}
