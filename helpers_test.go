package qflash

import (
	"encoding/binary"
	"os"
	"testing"
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
