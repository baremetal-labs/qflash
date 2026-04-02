package qflash

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

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

// --- Feature 4: extended L2 entries ---

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

// --- Feature 6: zstd decompression ---

// buildZstdQCOW2 constructs a minimal qcow2 v3 image with zstd-compressed
// clusters (compression_type=1, incompatCompression bit set).
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

	additionalSectors := uint64((len(compressed) + 511) / 512)
	if additionalSectors > 0 {
		additionalSectors--
	}
	compressedOffset := uint64(3 * clusterSize)
	x := uint(70 - clusterBits)
	l2EntryVal := (uint64(1) << 62) | (additionalSectors << x) | compressedOffset

	totalSectors := (additionalSectors + 1)
	compressedPadded := make([]byte, totalSectors*512)
	copy(compressedPadded, compressed)

	img := make([]byte, 3*clusterSize+len(compressedPadded))

	copy(img[0:4], "QFI\xfb")
	binary.BigEndian.PutUint32(img[4:8], 3)
	binary.BigEndian.PutUint32(img[20:24], clusterBits)
	binary.BigEndian.PutUint64(img[24:32], clusterSize)
	binary.BigEndian.PutUint32(img[36:40], 1)
	binary.BigEndian.PutUint64(img[40:48], clusterSize)
	binary.BigEndian.PutUint64(img[72:80], incompatCompression)
	binary.BigEndian.PutUint32(img[96:100], 4)
	binary.BigEndian.PutUint32(img[100:104], 112)
	img[104] = 1 // compression_type = zstd

	binary.BigEndian.PutUint64(img[clusterSize:], 0x8000000000000400)
	binary.BigEndian.PutUint64(img[2*clusterSize:], l2EntryVal)
	copy(img[3*clusterSize:], compressedPadded)

	return img
}

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
