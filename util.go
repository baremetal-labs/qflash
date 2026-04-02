package qflash

import (
	"fmt"
	"math"
)

// DebugMode enables verbose QCOW2 internal diagnostics.
var DebugMode bool

func HumanSize(bytes uint64) string {
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(bytes)/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// safeOffset converts a uint64 file offset to int64, returning an error if
// the value exceeds the maximum representable int64.
func safeOffset(v uint64) (int64, error) {
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("offset 0x%x exceeds maximum file offset", v)
	}
	return int64(v), nil // #nosec G115
}

func isZeroBuf(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
