//go:build !unix

package sqlite

// Ownership is a POSIX concept; elsewhere the error carries only the
// underlying failure.
func statOwner(string) ownership { return ownership{} }

func procOwner() ownership { return ownership{} }
