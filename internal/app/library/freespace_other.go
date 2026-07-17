//go:build !unix

package library

// freeBytes is unavailable on this platform.
func freeBytes(string) int64 { return 0 }
