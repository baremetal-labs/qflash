package qflash

import (
	"os"
	"testing"
)

// --- Feature 1: incompatible features check ---

func TestCheckIncompatibleFeatures_OK(t *testing.T) {
	h := &QCOWHeader{Version: 3, IncompatibleFeatures: 0}
	if err := checkIncompatibleFeatures(h); err != nil {
		t.Errorf("unexpected error for clean image: %v", err)
	}
}

func TestCheckIncompatibleFeatures_V2Skipped(t *testing.T) {
	// v2 has no incompat field; should always pass regardless of value
	h := &QCOWHeader{Version: 2, IncompatibleFeatures: 0xFF}
	if err := checkIncompatibleFeatures(h); err != nil {
		t.Errorf("v2 should skip incompat check, got: %v", err)
	}
}

func TestCheckIncompatibleFeatures_Fatal(t *testing.T) {
	cases := []struct {
		name string
		bits uint64
	}{
		{"corrupt", incompatCorrupt},
		{"external data", incompatExternalData},
		{"non-deflate compression", incompatCompression},
			{"unknown bit 5", 1 << 5},
	}
	for _, c := range cases {
		h := &QCOWHeader{Version: 3, IncompatibleFeatures: c.bits}
		if err := checkIncompatibleFeatures(h); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

// --- Feature 1b: dirty bit warning ---

func TestCheckIncompatibleFeatures_DirtyWarns(t *testing.T) {
	// Dirty bit should not return an error, but should not silently succeed
	// (the warning goes to stderr). Verify err == nil.
	h := &QCOWHeader{Version: 3, IncompatibleFeatures: incompatDirty}
	if err := checkIncompatibleFeatures(h); err != nil {
		t.Errorf("dirty bit should not be fatal, got: %v", err)
	}
}

// --- Feature 2: encryption detection ---

func TestCheckEncryption(t *testing.T) {
	if err := checkEncryption(&QCOWHeader{CryptMethod: 0}); err != nil {
		t.Errorf("no encryption: unexpected error: %v", err)
	}
	if err := checkEncryption(&QCOWHeader{CryptMethod: 1}); err == nil {
		t.Error("AES: expected error, got nil")
	}
	if err := checkEncryption(&QCOWHeader{CryptMethod: 2}); err == nil {
		t.Error("LUKS: expected error, got nil")
	}
	if err := checkEncryption(&QCOWHeader{CryptMethod: 99}); err == nil {
		t.Error("unknown method: expected error, got nil")
	}
}

// --- Unit tests (no I/O) ---

func TestParseL1Entry(t *testing.T) {
	cases := []struct {
		raw          uint64
		wantOffset   uint64
		wantNeedsCOW bool
	}{
		{0x8000000000000400, 0x0000000000000400, false}, // bit63=1 → no COW
		{0x0000000000000400, 0x0000000000000400, true},  // bit63=0 → COW
		{0x80000000000005FF, 0x0000000000000400, false}, // low bits masked out
		{0x0000000000000000, 0x0000000000000000, true},  // unallocated
	}
	for _, c := range cases {
		e := parseL1Entry(c.raw)
		if e.L2TableOffset != c.wantOffset {
			t.Errorf("parseL1Entry(0x%016x).L2TableOffset = 0x%x, want 0x%x",
				c.raw, e.L2TableOffset, c.wantOffset)
		}
		if e.NeedsCOW != c.wantNeedsCOW {
			t.Errorf("parseL1Entry(0x%016x).NeedsCOW = %v, want %v",
				c.raw, e.NeedsCOW, c.wantNeedsCOW)
		}
	}
}

func TestParseL2Entry_Standard(t *testing.T) {
	cases := []struct {
		raw        uint64
		wantOffset uint64
		wantZero   bool
	}{
		{0x8000000000000600, 0x0000000000000600, false}, // normal data cluster
		{0x0000000000000001, 0x0000000000000000, true},  // zero flag (bit 0)
		{0x0000000000000000, 0x0000000000000000, false}, // unallocated
	}
	for _, c := range cases {
		e := parseL2Entry(c.raw, 9)
		if e.IsCompressed {
			t.Errorf("parseL2Entry(0x%016x): unexpected IsCompressed", c.raw)
		}
		if e.HostClusterOffset != c.wantOffset {
			t.Errorf("parseL2Entry(0x%016x).HostClusterOffset = 0x%x, want 0x%x",
				c.raw, e.HostClusterOffset, c.wantOffset)
		}
		if e.IsZero != c.wantZero {
			t.Errorf("parseL2Entry(0x%016x).IsZero = %v, want %v",
				c.raw, e.IsZero, c.wantZero)
		}
	}
}

func TestParseL2Entry_Compressed(t *testing.T) {
	// bit 62 set → compressed cluster
	raw := uint64(1) << 62
	e := parseL2Entry(raw, 16)
	if !e.IsCompressed {
		t.Error("expected IsCompressed=true for bit62 set")
	}
}

func TestReadHeader(t *testing.T) {
	f := tempQCOW2(t, 0xAB)
	defer f.Close()

	h, err := readHeader(f)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}

	checks := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"Version", uint64(h.Version), 2},
		{"ClusterBits", uint64(h.ClusterBits), 9},
		{"ClusterSize", h.ClusterSize, 512},
		{"L1Size", uint64(h.L1Size), 1},
		{"L1TableOffset", h.L1TableOffset, 512},
		{"Size", h.Size, 512},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestReadHeader_BadMagic(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.bad")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(make([]byte, 128)) // all zeros — magic won't match

	if _, err := readHeader(f); err == nil {
		t.Error("expected error for bad magic, got nil")
	}
}
