package qflash

import "fmt"

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

func isZeroBuf(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
