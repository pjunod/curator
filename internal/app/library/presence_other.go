//go:build !unix

package library

import "os"

func presenceRootIdentity(os.FileInfo) string { return "" }
