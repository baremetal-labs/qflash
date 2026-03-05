package qflash

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// QCOWLayer is one image in a backing-file chain (overlay → ... → base).
type QCOWLayer struct {
	File    *os.File
	Header  *QCOWHeader
	L1      []L1Entry
	l2Cache sync.Map // key: uint64 byte offset → value: []L2Entry
}

// OpenLayerChain opens src and recursively opens any backing files, returning
// the full chain with the overlay first and the base image last.
func OpenLayerChain(src *os.File) ([]*QCOWLayer, error) {
	header, err := readHeader(src)
	if err != nil {
		return nil, err
	}
	if err := checkIncompatibleFeatures(header); err != nil {
		return nil, err
	}
	if err := checkEncryption(header); err != nil {
		return nil, err
	}
	l1, err := readL1Table(src, header)
	if err != nil {
		return nil, err
	}

	layer := &QCOWLayer{File: src, Header: header, L1: l1}
	chain := []*QCOWLayer{layer}

	if header.BackingFileOffset != 0 && header.BackingFileSize != 0 {
		nameBuf := make([]byte, header.BackingFileSize)
		if _, err := src.ReadAt(nameBuf, int64(header.BackingFileOffset)); err != nil {
			return nil, fmt.Errorf("reading backing file name: %v", err)
		}
		backingPath := string(nameBuf)
		if !filepath.IsAbs(backingPath) {
			backingPath = filepath.Join(filepath.Dir(src.Name()), backingPath)
		}

		bf, err := os.Open(backingPath)
		if err != nil {
			return nil, fmt.Errorf("opening backing file %q: %v", backingPath, err)
		}

		rest, err := OpenLayerChain(bf)
		if err != nil {
			bf.Close()
			return nil, fmt.Errorf("backing file %q: %v", backingPath, err)
		}
		chain = append(chain, rest...)
	}

	return chain, nil
}

func readL1Table(src *os.File, header *QCOWHeader) ([]L1Entry, error) {
	l1Raw := make([]byte, uint64(header.L1Size)*8)
	_, err := src.ReadAt(l1Raw, int64(header.L1TableOffset))
	if err != nil {
		return nil, fmt.Errorf("error reading L1 table: %v", err)
	}

	l1Entries := make([]L1Entry, header.L1Size)
	for i := uint32(0); i < header.L1Size; i++ {
		raw := binary.BigEndian.Uint64(l1Raw[i*8 : (i+1)*8])
		l1Entries[i] = parseL1Entry(raw)
	}
	return l1Entries, nil
}

func readL2Table(src *os.File, header *QCOWHeader, l2ByteOffset uint64) ([]L2Entry, error) {
	l2Raw := make([]byte, header.ClusterSize)
	_, err := src.ReadAt(l2Raw, int64(l2ByteOffset))
	if err != nil {
		return nil, fmt.Errorf("error reading L2 table at 0x%x: %v", l2ByteOffset, err)
	}

	l2Entries := int(header.ClusterSize / 8)
	entries := make([]L2Entry, l2Entries)
	for i := 0; i < l2Entries; i++ {
		raw := binary.BigEndian.Uint64(l2Raw[i*8 : (i+1)*8])
		entries[i] = parseL2Entry(raw, header.ClusterBits)
	}
	return entries, nil
}

// getL2Table returns the parsed L2 table at l2ByteOffset, consulting a
// per-layer in-memory cache first to avoid redundant disk reads during
// sequential cluster scans (a 64 KiB cluster holds 8192 L2 entries, so
// the same table is visited thousands of times when scanning linearly).
func (layer *QCOWLayer) getL2Table(l2ByteOffset uint64) ([]L2Entry, error) {
	if v, ok := layer.l2Cache.Load(l2ByteOffset); ok {
		return v.([]L2Entry), nil
	}
	entries, err := readL2Table(layer.File, layer.Header, l2ByteOffset)
	if err != nil {
		return nil, err
	}
	layer.l2Cache.Store(l2ByteOffset, entries)
	return entries, nil
}

// readCompressedCluster reads and decompresses a compressed cluster.
// Reads are sector-aligned (matching QEMU's block layer), then the
// compressed data slice is extracted starting at the intra-sector offset.
// Dispatches to deflate or zstd based on header.CompressionType.
func readCompressedCluster(src *os.File, header *QCOWHeader, l2 L2Entry) ([]byte, error) {
	// Align to 512-byte sector boundary, matching QEMU's block layer reads.
	sectorOffset := l2.CompressedOffset & 511
	alignedOffset := l2.CompressedOffset - sectorOffset
	readSize := (l2.AdditionalSectors + 1) * 512

	raw := make([]byte, readSize)
	n, err := src.ReadAt(raw, int64(alignedOffset))
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("error reading compressed data at offset 0x%x: %v", alignedOffset, err)
	}
	if n == 0 {
		fi, _ := src.Stat()
		var fileSize int64
		if fi != nil {
			fileSize = fi.Size()
		}
		return nil, fmt.Errorf("zero bytes read at compressed offset 0x%x (file size on disk: %d): initramfs file is truncated — QCOW2 too large to embed", alignedOffset, fileSize)
	}

	// Compressed data starts at sectorOffset within raw.
	compressed := raw[sectorOffset:]
	out := make([]byte, header.ClusterSize)

	switch header.CompressionType {
	case 0: // raw deflate — no zlib header; create a fresh reader per cluster
		r := flate.NewReader(bytes.NewReader(compressed))
		_, err = io.ReadFull(r, out)
		r.Close()
		if err != nil && err != io.ErrUnexpectedEOF {
			n := 32
			if len(compressed) < n {
				n = len(compressed)
			}
			return nil, fmt.Errorf("error decompressing deflate cluster: %v\n  CompressedOffset=0x%x sectorOffset=%d AdditionalSectors=%d readSize=%d\n  first %d compressed bytes: %x",
				err, l2.CompressedOffset, sectorOffset, l2.AdditionalSectors, readSize, n, compressed[:n])
		}

	case 1: // zstd
		dec, err := zstd.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, fmt.Errorf("error creating zstd reader: %v", err)
		}
		_, err = io.ReadFull(dec, out)
		dec.Close()
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("error decompressing zstd cluster: %v", err)
		}

	default:
		return nil, fmt.Errorf("unsupported compression type %d", header.CompressionType)
	}

	return out, nil
}

// readVirtualCluster resolves virtualOffset in a single image layer.
// Returns:
//   - (nil, 0, nil)            cluster is unallocated in this layer
//   - (data, 0xFFFFFFFF, nil)  standard cluster; all 32 subclusters provided
//   - (data, mask, nil)        extended L2; mask bit i = 1 means subcluster i
//     was provided by this layer (allocated or zero-flagged)
func readVirtualCluster(layer *QCOWLayer, virtualOffset uint64) ([]byte, uint32, error) {
	header := layer.Header
	clusterBits := header.ClusterBits
	clusterSize := header.ClusterSize

	var l2Bits, l2Entries uint64
	if header.ExtL2 {
		l2Bits = uint64(clusterBits - 4) // each entry is 16 bytes → cluster_size/16 entries
		l2Entries = clusterSize / 16
	} else {
		l2Bits = uint64(clusterBits - 3)
		l2Entries = clusterSize / 8
	}

	l2Index := (virtualOffset >> clusterBits) & (l2Entries - 1)
	l1Index := virtualOffset >> (uint64(clusterBits) + l2Bits)

	if l1Index >= uint64(len(layer.L1)) {
		return nil, 0, fmt.Errorf("L1 index %d out of range (%d entries)", l1Index, len(layer.L1))
	}

	l1Entry := layer.L1[l1Index]
	if l1Entry.L2TableOffset == 0 {
		return nil, 0, nil // L2 table not allocated
	}

	if header.ExtL2 {
		return readVirtualClusterExtL2(layer, l1Entry.L2TableOffset, l2Index)
	}

	l2Table, err := layer.getL2Table(l1Entry.L2TableOffset)
	if err != nil {
		return nil, 0, err
	}

	l2Entry := l2Table[l2Index]

	switch {
	case l2Entry.IsCompressed:
		buf, err := readCompressedCluster(layer.File, header, l2Entry)
		return buf, 0xFFFFFFFF, err

	case l2Entry.IsZero:
		return make([]byte, clusterSize), 0xFFFFFFFF, nil

	case l2Entry.HostClusterOffset == 0 && (l2Entry.RawValue>>63)&1 == 0:
		return nil, 0, nil // unallocated

	default:
		buf := make([]byte, clusterSize)
		_, err := layer.File.ReadAt(buf, int64(l2Entry.HostClusterOffset))
		if err != nil {
			return nil, 0, fmt.Errorf("error reading host cluster at 0x%x: %v", l2Entry.HostClusterOffset, err)
		}
		return buf, 0xFFFFFFFF, nil
	}
}

// readVirtualClusterExtL2 reads a cluster from an image using extended 128-bit
// L2 entries. Each entry's second word carries a per-subcluster allocation
// bitmap (bits 0-31) and a per-subcluster zero bitmap (bits 32-63).
func readVirtualClusterExtL2(layer *QCOWLayer, l2TableOffset, l2Index uint64) ([]byte, uint32, error) {
	header := layer.Header
	clusterSize := header.ClusterSize

	raw := make([]byte, 16)
	if _, err := layer.File.ReadAt(raw, int64(l2TableOffset+l2Index*16)); err != nil {
		return nil, 0, fmt.Errorf("reading ext L2 entry: %v", err)
	}
	word0 := binary.BigEndian.Uint64(raw[0:8])
	word1 := binary.BigEndian.Uint64(raw[8:16])

	// Bit 62 of word0 = compressed cluster.
	if (word0>>62)&1 != 0 {
		l2Entry := parseL2Entry(word0, header.ClusterBits)
		buf, err := readCompressedCluster(layer.File, header, l2Entry)
		return buf, 0xFFFFFFFF, err
	}

	hostOffset := word0 & 0x00FFFFFFFFFFFE00
	allocBits := uint32(word1)      // bits  0-31: subcluster i is allocated
	zeroBits := uint32(word1 >> 32) // bits 32-63: subcluster i reads as zero

	// filledMask: subclusters handled by this layer (allocated OR zero-flagged).
	filledMask := allocBits | zeroBits

	// Fully unallocated: no subclusters provided and host offset is absent.
	if filledMask == 0 && hostOffset == 0 && (word0>>63)&1 == 0 {
		return nil, 0, nil
	}

	result := make([]byte, clusterSize) // zero-initialised; zero subclusters are already handled
	subclusterSize := clusterSize / 32

	if hostOffset != 0 && allocBits != 0 {
		hostData := make([]byte, clusterSize)
		if _, err := layer.File.ReadAt(hostData, int64(hostOffset)); err != nil {
			return nil, 0, fmt.Errorf("reading ext L2 host cluster at 0x%x: %v", hostOffset, err)
		}
		for i := uint32(0); i < 32; i++ {
			if allocBits&(1<<i) != 0 {
				start := uint64(i) * subclusterSize
				copy(result[start:], hostData[start:start+subclusterSize])
			}
		}
	}

	return result, filledMask, nil
}

// readVirtualClusterChain reads a virtual cluster by walking the layer chain.
// If a layer does not have the cluster allocated, the next layer is tried.
// If no layer has it, zeros are returned (per spec: fully unallocated = zeros).
func readVirtualClusterChain(chain []*QCOWLayer, virtualOffset uint64) ([]byte, error) {
	clusterSize := chain[0].Header.ClusterSize
	subclusterSize := clusterSize / 32

	// result accumulates data across layers; starts as zeros (fully-unallocated = zeros per spec).
	result := make([]byte, clusterSize)
	// pendingMask tracks which subclusters still need data from a deeper layer.
	pendingMask := uint32(0xFFFFFFFF)

	for _, layer := range chain {
		if virtualOffset >= layer.Header.Size {
			continue
		}
		data, filledMask, err := readVirtualCluster(layer, virtualOffset)
		if err != nil {
			return nil, err
		}
		if data == nil {
			continue
		}
		if !layer.Header.ExtL2 {
			// Standard cluster: entire cluster provided by this layer.
			return data, nil
		}
		// Extended L2: merge only the subclusters this layer provides that are still pending.
		for i := uint32(0); i < 32; i++ {
			bit := uint32(1) << i
			if filledMask&bit != 0 && pendingMask&bit != 0 {
				start := uint64(i) * subclusterSize
				copy(result[start:], data[start:start+subclusterSize])
				pendingMask &^= bit
			}
		}
		if pendingMask == 0 {
			break
		}
	}

	return result, nil
}
