package qflash

import (
	"bytes"
	"os"
	"testing"
)

func TestWriteToDevice(t *testing.T) {
	const fill = byte(0xCD)
	src := tempQCOW2(t, fill)
	defer src.Close()

	chain, err := OpenLayerChain(src)
	if err != nil {
		t.Fatalf("OpenLayerChain: %v", err)
	}

	dst, err := os.CreateTemp(t.TempDir(), "*.raw")
	if err != nil {
		t.Fatalf("CreateTemp dst: %v", err)
	}
	defer dst.Close()

	if err := WriteToDevice(chain, dst); err != nil {
		t.Fatalf("WriteToDevice: %v", err)
	}

	got := make([]byte, 512)
	if _, err := dst.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{fill}, 512)) {
		t.Errorf("dst[0] = 0x%02x, want 0x%02x", got[0], fill)
	}
}
