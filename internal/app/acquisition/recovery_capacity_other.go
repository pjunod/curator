//go:build !darwin && !linux && !freebsd

package acquisition

import "fmt"

func recoveryCapacity(_ string, _ int64) error {
	return fmt.Errorf("library capacity could not be verified on this platform")
}
