// Package qflash reads QCOW2 virtual disk images and writes them to block devices or files.
package qflash

import (
	"encoding/binary"
	"fmt"
	"os"
)

type QCOWHeader struct {
	Magic                 string
	Version               uint32
	BackingFileOffset     uint64
	BackingFileSize       uint32
	ClusterBits           uint32
	Size                  uint64
	CryptMethod           uint32
	ClusterSize           uint64
	L1Size                uint32
	L1TableOffset         uint64
	RefcountTableOffset   uint64
	RefcountTableClusters uint32
	NbSnapshots           uint32
	SnapshotsOffset       uint64
	IncompatibleFeatures  uint64
	CompatibleFeatures    uint64
	AutoclearFeatures     uint64
	RefcountOrder         uint32
	HeaderLength          uint32
	ExtL2                 bool  // true when incompatible feature bit 4 is set (128-bit L2 entries)
	CompressionType       uint8 // 0=deflate (default), 1=zstd; present at header offset 104 (v3+)
}

// L1Entry holds a parsed L1 table entry.
// L2TableOffset is a direct byte offset into the image file.
// NeedsCOW is true when bit 63 of the raw entry is 0 (refcount != 1).
type L1Entry struct {
	L2TableOffset uint64
	NeedsCOW      bool
}

// L2Entry holds a parsed L2 table entry for a guest cluster.
type L2Entry struct {
	// Standard cluster fields
	HostClusterOffset uint64 // direct byte offset; 0 = unallocated
	IsZero            bool   // cluster reads as all zeros (bit 0 of standard descriptor)

	// Compressed cluster fields
	IsCompressed      bool
	CompressedOffset  uint64
	AdditionalSectors uint64

	RawValue uint64
}

// Incompatible feature bit positions (v3+).
const (
	incompatDirty        = uint64(1 << 0) // refcounts may be inconsistent
	incompatCorrupt      = uint64(1 << 1) // image is corrupt, must not use
	incompatExternalData = uint64(1 << 2) // cluster data in separate file
	incompatCompression  = uint64(1 << 3) // non-deflate compression in use
	incompatExtL2        = uint64(1 << 4) // extended (128-bit) L2 entries

	incompatKnownBits = incompatDirty | incompatCorrupt | incompatExternalData |
		incompatCompression | incompatExtL2
)

func readHeader(src *os.File) (*QCOWHeader, error) {
	header := make([]byte, 112) // 104 base + 8 bytes for optional compression_type + padding
	n, err := src.ReadAt(header, 0)
	if err != nil && n < 104 {
		return nil, fmt.Errorf("error reading header: %v", err)
	}
	header = header[:n]

	magic := string(header[0:4])
	if magic != "QFI\xfb" {
		return nil, fmt.Errorf("invalid magic: %q (expected QFI\\xfb)", magic)
	}

	qcowHeader := &QCOWHeader{
		Magic:                 magic,
		Version:               binary.BigEndian.Uint32(header[4:8]),
		BackingFileOffset:     binary.BigEndian.Uint64(header[8:16]),
		BackingFileSize:       binary.BigEndian.Uint32(header[16:20]),
		ClusterBits:           binary.BigEndian.Uint32(header[20:24]),
		Size:                  binary.BigEndian.Uint64(header[24:32]),
		CryptMethod:           binary.BigEndian.Uint32(header[32:36]),
		L1Size:                binary.BigEndian.Uint32(header[36:40]),
		L1TableOffset:         binary.BigEndian.Uint64(header[40:48]),
		RefcountTableOffset:   binary.BigEndian.Uint64(header[48:56]),
		RefcountTableClusters: binary.BigEndian.Uint32(header[56:60]),
		NbSnapshots:           binary.BigEndian.Uint32(header[60:64]),
		SnapshotsOffset:       binary.BigEndian.Uint64(header[64:72]),
	}

	if qcowHeader.Version >= 3 {
		qcowHeader.IncompatibleFeatures = binary.BigEndian.Uint64(header[72:80])
		qcowHeader.CompatibleFeatures = binary.BigEndian.Uint64(header[80:88])
		qcowHeader.AutoclearFeatures = binary.BigEndian.Uint64(header[88:96])
		qcowHeader.RefcountOrder = binary.BigEndian.Uint32(header[96:100])
		qcowHeader.HeaderLength = binary.BigEndian.Uint32(header[100:104])
	} else {
		qcowHeader.HeaderLength = 72
	}

	qcowHeader.ClusterSize = 1 << qcowHeader.ClusterBits
	qcowHeader.ExtL2 = qcowHeader.IncompatibleFeatures&incompatExtL2 != 0

	// compression_type is at offset 104 (v3+), present only when header_length >= 105.
	if qcowHeader.Version >= 3 && qcowHeader.HeaderLength >= 105 && len(header) >= 105 {
		qcowHeader.CompressionType = header[104]
	}

	return qcowHeader, nil
}

// checkIncompatibleFeatures validates the incompatible feature flags for v3+
// images. Returns an error for any bit that would cause incorrect behaviour,
// and prints a warning for the dirty bit.
func checkIncompatibleFeatures(header *QCOWHeader) error {
	if header.Version < 3 {
		return nil
	}

	f := header.IncompatibleFeatures

	if f&incompatCorrupt != 0 {
		return fmt.Errorf("image has the corrupt bit set: data structures may be damaged, refusing to open")
	}
	if f&incompatExternalData != 0 {
		return fmt.Errorf("image uses an external data file which is not supported")
	}
	if f&incompatCompression != 0 {
		switch header.CompressionType {
		case 1: // zstd — supported
		default:
			return fmt.Errorf("image uses an unsupported compression type %d (only deflate and zstd are supported)", header.CompressionType)
		}
	}
	if unknown := f &^ incompatKnownBits; unknown != 0 {
		return fmt.Errorf("image has unknown incompatible feature bits set: 0x%x (refusing per spec)", unknown)
	}
	if f&incompatDirty != 0 {
		fmt.Fprintf(os.Stderr, "warning: image has the dirty bit set — refcounts may be inconsistent\n")
	}

	return nil
}

// checkEncryption returns an error if the image uses encryption, since we
// cannot decrypt without a key.
func checkEncryption(header *QCOWHeader) error {
	switch header.CryptMethod {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("image uses AES encryption: decryption is not supported")
	case 2:
		return fmt.Errorf("image uses LUKS encryption: decryption is not supported")
	default:
		return fmt.Errorf("image uses unknown encryption method %d", header.CryptMethod)
	}
}

// parseL1Entry parses a raw 64-bit L1 table entry.
// Per spec bits 9-55 are bits 9-55 of the byte offset to the L2 table.
// Masking with 0x00FFFFFFFFFFFE00 extracts the direct byte offset.
// Bit 63 = 1 means refcount is exactly 1 (no COW needed).
func parseL1Entry(entry uint64) L1Entry {
	return L1Entry{
		L2TableOffset: entry & 0x00FFFFFFFFFFFE00,
		NeedsCOW:      (entry>>63)&1 == 0,
	}
}

// parseL2Entry parses a raw 64-bit L2 table entry.
// For standard clusters, bits 9-55 are the direct host byte offset.
// For compressed clusters the layout depends on cluster_bits.
func parseL2Entry(entry uint64, clusterBits uint32) L2Entry {
	isCompressed := (entry>>62)&1 != 0

	if isCompressed {
		// x = 62 - (cluster_bits - 8) = 70 - cluster_bits
		x := uint(70 - clusterBits)
		offsetMask := (uint64(1) << x) - 1
		sectorBits := uint(62) - x
		sectorMask := (uint64(1) << sectorBits) - 1
		return L2Entry{
			IsCompressed:      true,
			CompressedOffset:  entry & offsetMask,
			AdditionalSectors: (entry >> x) & sectorMask,
			RawValue:          entry,
		}
	}

	return L2Entry{
		HostClusterOffset: entry & 0x00FFFFFFFFFFFE00,
		IsZero:            (entry & 1) != 0,
		RawValue:          entry,
	}
}
