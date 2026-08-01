// Package downloadpriority defines the priority bands shared with nzbd.
package downloadpriority

// Canonical scheduler priority values. Higher values are selected first.
const (
	VeryLow  = -100
	Low      = -50
	Normal   = 0
	High     = 50
	VeryHigh = 100
	Force    = 900
)

// Valid reports whether value is one of the supported user-facing bands.
func Valid(value int) bool {
	switch value {
	case VeryLow, Low, Normal, High, VeryHigh, Force:
		return true
	default:
		return false
	}
}

// Label renders a priority for logs and API validation messages.
func Label(value int) string {
	switch value {
	case VeryLow:
		return "very low"
	case Low:
		return "low"
	case Normal:
		return "normal"
	case High:
		return "high"
	case VeryHigh:
		return "very high"
	case Force:
		return "force"
	default:
		return "custom"
	}
}
