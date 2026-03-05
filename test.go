package qflash

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildMinimalQCOW2 constructs a valid minimal qcow2 v2 image in memory.
//
// Layout (cluster_bits=9, cluster_size=512 bytes):
//
//	Cluster 0 (0x000): Header
//	Cluster 1 (0x200): L1 table  — 1 entry pointing to L2 at 0x400
//	Cluster 2 (0x400): L2 table  — entry 0 pointing to data at 0x600
//	Cluster 3 (0x600): Data      — 512 bytes of fill
func buildMinimalQCOW2(fill byte) []byte {
	const clusterBits = 9
	const clusterSize = 1 << clusterBits // 512

	img := make([]byte, 4*clusterSize)

	// Header (cluster 0)
	copy(img[0:4], "QFI\xfb")                            // magic
	binary.BigEndian.PutUint32(img[4:8], 2)               // version
	binary.BigEndian.PutUint32(img[20:24], clusterBits)   // cluster_bits
	binary.BigEndian.PutUint64(img[24:32], clusterSize)   // virtual disk size = 1 cluster
	binary.BigEndian.PutUint32(img[36:40], 1)             // l1_size
	binary.BigEndian.PutUint64(img[40:48], clusterSize)   // l1_table_offset = cluster 1 (0x200)

	// L1 table (cluster 1, offset 0x200)
	// Entry: L2 table at byte offset 0x400 (cluster 2), bit 63=1 (refcount==1)
	binary.BigEndian.PutUint64(img[clusterSize:], 0x8000000000000400)

	// L2 table (cluster 2, offset 0x400)
	// Entry 0: data at byte offset 0x600 (cluster 3), bit 63=1 (refcount==1)
	binary.BigEndian.PutUint64(img[2*clusterSize:], 0x8000000000000600)

	// Data (cluster 3, offset 0x600)
	for i := 3 * clusterSize; i < 4*clusterSize; i++ {
		img[i] = fill
	}

	return img
}

// buildMinimalQCOW2WithSize builds a two-cluster qcow2 image.
// Cluster 0 gets fill0, cluster 1 gets fill1. virtualSize is the header Size field.
//
// Layout (cluster_bits=9):
//
//	Cluster 0 (0x000): Header
//	Cluster 1 (0x200): L1 table  → L2 at 0x400
//	Cluster 2 (0x400): L2 table  → data0 at 0x600, data1 at 0x800
//	Cluster 3 (0x600): Data 0    (fill0)
//	Cluster 4 (0x800): Data 1    (fill1)
func buildMinimalQCOW2WithSize(fill0, fill1 byte, virtualSize uint64) []byte {
	const clusterBits = 9
	const clusterSize = 1 << clusterBits

	img := make([]byte, 5*clusterSize)

	copy(img[0:4], "QFI\xfb")
	binary.BigEndian.PutUint32(img[4:8], 2)
	binary.BigEndian.PutUint32(img[20:24], clusterBits)
	binary.BigEndian.PutUint64(img[24:32], virtualSize)
	binary.BigEndian.PutUint32(img[36:40], 1)
	binary.BigEndian.PutUint64(img[40:48], clusterSize) // l1 at cluster 1

	binary.BigEndian.PutUint64(img[clusterSize:], 0x8000000000000400) // L2 at 0x400

	binary.BigEndian.PutUint64(img[2*clusterSize:], 0x8000000000000600)   // data0 at 0x600
	binary.BigEndian.PutUint64(img[2*clusterSize+8:], 0x8000000000000800) // data1 at 0x800

	for i := 3 * clusterSize; i < 4*clusterSize; i++ {
		img[i] = fill0
	}
	for i := 4 * clusterSize; i < 5*clusterSize; i++ {
		img[i] = fill1
	}
	return img
}

// buildOverlayQCOW2 builds an overlay image where cluster 0 holds fill0 and
// cluster 1 is unallocated (so it falls through to the named backing file).
//
// Layout (cluster_bits=9):
//
//	Cluster 0 (0x000): Header + backing file path at byte 72
//	Cluster 1 (0x200): L1 table  → L2 at 0x400
//	Cluster 2 (0x400): L2 table  → data0 at 0x600; L2[1]=0 (unallocated)
//	Cluster 3 (0x600): Data 0    (fill0)
func buildOverlayQCOW2(fill0 byte, backingPath string) []byte {
	const clusterBits = 9
	const clusterSize = 1 << clusterBits

	img := make([]byte, 4*clusterSize)

	copy(img[0:4], "QFI\xfb")
	binary.BigEndian.PutUint32(img[4:8], 2)
	binary.BigEndian.PutUint64(img[8:16], 72)                           // backing_file_offset
	binary.BigEndian.PutUint32(img[16:20], uint32(len(backingPath)))    // backing_file_size
	binary.BigEndian.PutUint32(img[20:24], clusterBits)
	binary.BigEndian.PutUint64(img[24:32], 1024)                        // virtual size = 2 clusters
	binary.BigEndian.PutUint32(img[36:40], 1)
	binary.BigEndian.PutUint64(img[40:48], clusterSize)                 // l1 at cluster 1
	copy(img[72:], backingPath)

	binary.BigEndian.PutUint64(img[clusterSize:], 0x8000000000000400)   // L2 at 0x400
	binary.BigEndian.PutUint64(img[2*clusterSize:], 0x8000000000000600) // data0 at 0x600
	// L2[1] = 0 → unallocated, falls through to backing file

	for i := 3 * clusterSize; i < 4*clusterSize; i++ {
		img[i] = fill0
	}
	return img
}

func tempQCOW2(t *testing.T, fill byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.qcow2")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.Write(buildMinimalQCOW2(fill)); err != nil {
		t.Fatalf("write synthetic image: %v", err)
	}
	return f
}

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

// --- Integration tests (synthetic image file) ---

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

func TestReadVirtualCluster_Data(t *testing.T) {
	const fill = byte(0xAB)
	f := tempQCOW2(t, fill)
	defer f.Close()

	h, err := readHeader(f)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	l1, err := readL1Table(f, h)
	if err != nil {
		t.Fatalf("readL1Table: %v", err)
	}

	layer := &QCOWLayer{File: f, Header: h, L1: l1}
	data, _, err := readVirtualCluster(layer, 0)
	if err != nil {
		t.Fatalf("readVirtualCluster: %v", err)
	}
	if data == nil {
		t.Fatal("got nil (unallocated) for cluster that should have data")
	}
	if len(data) != 512 {
		t.Fatalf("len(data) = %d, want 512", len(data))
	}
	if !bytes.Equal(data, bytes.Repeat([]byte{fill}, 512)) {
		t.Errorf("data[0] = 0x%02x, want 0x%02x", data[0], fill)
	}
}

func TestReadVirtualCluster_Unallocated(t *testing.T) {
	f := tempQCOW2(t, 0xAB)
	defer f.Close()

	h, err := readHeader(f)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	l1, err := readL1Table(f, h)
	if err != nil {
		t.Fatalf("readL1Table: %v", err)
	}

	// Virtual offset 512 maps to L2[1], which is zero in our image → unallocated.
	layer := &QCOWLayer{File: f, Header: h, L1: l1}
	data, _, err := readVirtualCluster(layer, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Errorf("expected nil for unallocated cluster, got %d bytes", len(data))
	}
}

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

// buildExtL2QCOW2 constructs a valid minimal qcow2 v3 image with the
// ExtL2 incompatible feature bit set (128-bit L2 entries).
//
// Layout (cluster_bits=9, cluster_size=512 bytes):
//
//	Cluster 0 (0x000): Header (v3, IncompatFeatures=incompatExtL2)
//	Cluster 1 (0x200): L1 table  — 1 entry pointing to L2 at 0x400
//	Cluster 2 (0x400): L2 table  — 32 extended entries (16 bytes each)
//	                    entry 0 word0=host@0x600, word1 encodes:
//	                      allocBits=0x0000FFFF (subclusters 0-15 allocated)
//	                      zeroBits =0xFFFF0000 (subclusters 16-31 zero)
//	Cluster 3 (0x600): Data      — 512 bytes of fill
//
// The expected read result is:
//
//	bytes   0-255: fill  (subclusters 0-15, allocated)
//	bytes 256-511: 0x00  (subclusters 16-31, zero-flagged)
func buildExtL2QCOW2(fill byte) []byte {
	const clusterBits = 9
	const clusterSize = 1 << clusterBits // 512

	img := make([]byte, 4*clusterSize)

	// Header (cluster 0, v3 + ExtL2)
	copy(img[0:4], "QFI\xfb")
	binary.BigEndian.PutUint32(img[4:8], 3)                   // version
	binary.BigEndian.PutUint32(img[20:24], clusterBits)        // cluster_bits
	binary.BigEndian.PutUint64(img[24:32], clusterSize)        // virtual disk size
	binary.BigEndian.PutUint32(img[36:40], 1)                  // l1_size
	binary.BigEndian.PutUint64(img[40:48], clusterSize)        // l1_table_offset = 0x200
	binary.BigEndian.PutUint64(img[72:80], incompatExtL2)      // incompat features
	binary.BigEndian.PutUint32(img[96:100], 4)                 // refcount_order
	binary.BigEndian.PutUint32(img[100:104], 104)              // header_length

	// L1 table (cluster 1, offset 0x200): L2 at 0x400, bit63=1 (no COW)
	binary.BigEndian.PutUint64(img[clusterSize:], 0x8000000000000400)

	// L2 table (cluster 2, offset 0x400): extended 16-byte entries.
	// Entry 0:
	//   word0 = 0x8000000000000600  (host cluster at 0x600, bit63=1)
	//   word1 = 0xFFFF00000000FFFF
	//     low  32 bits → allocBits = 0x0000FFFF: subclusters 0-15 allocated
	//     high 32 bits → zeroBits  = 0xFFFF0000: subclusters 16-31 zero-flagged
	binary.BigEndian.PutUint64(img[2*clusterSize:], 0x8000000000000600)
	binary.BigEndian.PutUint64(img[2*clusterSize+8:], 0xFFFF00000000FFFF)

	// Data cluster (cluster 3, offset 0x600)
	for i := 3 * clusterSize; i < 4*clusterSize; i++ {
		img[i] = fill
	}

	return img
}

// --- Feature 4: extended L2 entries ---

func TestReadVirtualCluster_ExtL2(t *testing.T) {
	const fill = byte(0xAB)
	const clusterSize = 512
	const subclusterSize = clusterSize / 32 // 16 bytes per subcluster

	f, err := os.CreateTemp(t.TempDir(), "*.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(buildExtL2QCOW2(fill)); err != nil {
		t.Fatal(err)
	}

	h, err := readHeader(f)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if !h.ExtL2 {
		t.Fatal("expected ExtL2=true")
	}

	l1, err := readL1Table(f, h)
	if err != nil {
		t.Fatalf("readL1Table: %v", err)
	}

	layer := &QCOWLayer{File: f, Header: h, L1: l1}
	data, filledMask, err := readVirtualCluster(layer, 0)
	if err != nil {
		t.Fatalf("readVirtualCluster: %v", err)
	}
	if data == nil {
		t.Fatal("got nil (unallocated) for ExtL2 cluster that should have data")
	}
	if len(data) != clusterSize {
		t.Fatalf("len(data) = %d, want %d", len(data), clusterSize)
	}

	// allocBits=0x0000FFFF | zeroBits=0xFFFF0000 → filledMask=0xFFFFFFFF
	if filledMask != 0xFFFFFFFF {
		t.Errorf("filledMask = 0x%08x, want 0xFFFFFFFF", filledMask)
	}

	// Subclusters 0-15: allocated → should contain fill bytes
	for sc := 0; sc < 16; sc++ {
		start := sc * subclusterSize
		for j := 0; j < subclusterSize; j++ {
			if data[start+j] != fill {
				t.Errorf("subcluster %d byte %d = 0x%02x, want 0x%02x", sc, j, data[start+j], fill)
				break
			}
		}
	}

	// Subclusters 16-31: zero-flagged → should be zero
	for sc := 16; sc < 32; sc++ {
		start := sc * subclusterSize
		for j := 0; j < subclusterSize; j++ {
			if data[start+j] != 0 {
				t.Errorf("subcluster %d byte %d = 0x%02x, want 0x00", sc, j, data[start+j])
				break
			}
		}
	}
}

// buildZstdQCOW2 constructs a minimal qcow2 v3 image with zstd-compressed
// clusters (compression_type=1, incompatCompression bit set).
//
// Layout (cluster_bits=9, cluster_size=512):
//
//	Cluster 0 (0x000): Header (v3, incompat=incompatCompression, compression_type=1)
//	Cluster 1 (0x200): L1 table  → L2 at 0x400
//	Cluster 2 (0x400): L2 table  → compressed data starting at 0x600
//	Cluster 3 (0x600): zstd-compressed cluster data
func buildZstdQCOW2(t *testing.T, fill byte) []byte {
	t.Helper()
	const clusterBits = 9
	const clusterSize = 1 << clusterBits // 512

	// Compress a full cluster of fill bytes with zstd.
	plain := bytes.Repeat([]byte{fill}, clusterSize)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(plain, nil)
	enc.Close()

	// Compressed cluster descriptor: bit 62 set, x = 70 - clusterBits = 61.
	// AdditionalSectors = ceil(len(compressed)/512) - 1 sectors beyond the first.
	additionalSectors := uint64((len(compressed) + 511) / 512)
	if additionalSectors > 0 {
		additionalSectors-- // "additional" = total sectors - 1
	}
	compressedOffset := uint64(3 * clusterSize) // where compressed data lives
	// x = 70 - clusterBits = 61
	x := uint(70 - clusterBits)
	l2EntryVal := (uint64(1) << 62) | (additionalSectors << x) | compressedOffset

	// Allocate image: header + L1 + L2 + compressed data (padded to sector)
	totalSectors := (additionalSectors + 1)
	compressedPadded := make([]byte, totalSectors*512)
	copy(compressedPadded, compressed)

	img := make([]byte, 3*clusterSize+len(compressedPadded))

	// Header
	copy(img[0:4], "QFI\xfb")
	binary.BigEndian.PutUint32(img[4:8], 3)                         // version
	binary.BigEndian.PutUint32(img[20:24], clusterBits)
	binary.BigEndian.PutUint64(img[24:32], clusterSize)             // virtual size
	binary.BigEndian.PutUint32(img[36:40], 1)                       // l1_size
	binary.BigEndian.PutUint64(img[40:48], clusterSize)             // l1 at 0x200
	binary.BigEndian.PutUint64(img[72:80], incompatCompression)     // incompat: compression type bit
	binary.BigEndian.PutUint32(img[96:100], 4)                      // refcount_order
	binary.BigEndian.PutUint32(img[100:104], 112)                   // header_length (must be multiple of 8, ≥ 105)
	img[104] = 1                                                     // compression_type = zstd

	// L1 → L2 at 0x400
	binary.BigEndian.PutUint64(img[clusterSize:], 0x8000000000000400)

	// L2[0] → compressed cluster descriptor
	binary.BigEndian.PutUint64(img[2*clusterSize:], l2EntryVal)

	// Compressed data at cluster 3
	copy(img[3*clusterSize:], compressedPadded)

	return img
}

// --- Feature 6: zstd decompression ---

func TestReadVirtualCluster_Zstd(t *testing.T) {
	const fill = byte(0xDE)
	const clusterSize = 512

	f, err := os.CreateTemp(t.TempDir(), "*.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(buildZstdQCOW2(t, fill)); err != nil {
		t.Fatal(err)
	}

	h, err := readHeader(f)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if h.CompressionType != 1 {
		t.Fatalf("CompressionType = %d, want 1 (zstd)", h.CompressionType)
	}
	if err := checkIncompatibleFeatures(h); err != nil {
		t.Fatalf("checkIncompatibleFeatures: %v", err)
	}

	l1, err := readL1Table(f, h)
	if err != nil {
		t.Fatalf("readL1Table: %v", err)
	}

	layer := &QCOWLayer{File: f, Header: h, L1: l1}
	data, _, err := readVirtualCluster(layer, 0)
	if err != nil {
		t.Fatalf("readVirtualCluster: %v", err)
	}
	if data == nil {
		t.Fatal("got nil for zstd-compressed cluster")
	}
	if len(data) != clusterSize {
		t.Fatalf("len(data) = %d, want %d", len(data), clusterSize)
	}
	if !bytes.Equal(data, bytes.Repeat([]byte{fill}, clusterSize)) {
		t.Errorf("data[0] = 0x%02x, want 0x%02x", data[0], fill)
	}
}

func TestBackingFileChain(t *testing.T) {
	dir := t.TempDir()

	// Base image: cluster 0 = 0xBB, cluster 1 = 0xCC
	base := buildMinimalQCOW2WithSize(0xBB, 0xCC, 1024) // 2 virtual clusters
	baseFile, err := os.CreateTemp(dir, "base-*.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseFile.Write(base); err != nil {
		t.Fatal(err)
	}
	baseFile.Close()

	// Overlay: cluster 0 = 0xAA, cluster 1 = unallocated (should come from base)
	overlay := buildOverlayQCOW2(0xAA, baseFile.Name())
	overlayFile, err := os.CreateTemp(dir, "overlay-*.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := overlayFile.Write(overlay); err != nil {
		t.Fatal(err)
	}
	defer overlayFile.Close()

	chain, err := OpenLayerChain(overlayFile)
	if err != nil {
		t.Fatalf("OpenLayerChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}

	// Cluster 0: overlay has it → 0xAA
	d0, err := readVirtualClusterChain(chain, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d0[0] != 0xAA {
		t.Errorf("cluster 0: got 0x%02x, want 0xAA", d0[0])
	}

	// Cluster 1: overlay unallocated → falls through to base → 0xCC
	d1, err := readVirtualClusterChain(chain, 512)
	if err != nil {
		t.Fatal(err)
	}
	if d1[0] != 0xCC {
		t.Errorf("cluster 1: got 0x%02x, want 0xCC (from base)", d1[0])
	}
}
