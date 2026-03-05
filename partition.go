package qflash

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// PartitionTarget maps a partition number (1-based) to a destination device or file.
type PartitionTarget struct {
	Num    int
	Device string
}

// PartitionInfo describes a partition in the virtual disk.
type PartitionInfo struct {
	Num       int
	StartByte uint64
	SizeBytes uint64
}

// GetPartitionTable reads the MBR or GPT partition table from the virtual disk.
func GetPartitionTable(chain []*QCOWLayer) ([]PartitionInfo, error) {
	mbr, err := ReadVirtualBytes(chain, 0, 512)
	if err != nil {
		return nil, fmt.Errorf("reading MBR: %v", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xAA {
		return nil, fmt.Errorf("no MBR signature")
	}
	for i := 0; i < 4; i++ {
		if mbr[446+i*16+4] == 0xEE {
			return getGPTPartitionTable(chain)
		}
	}
	return getMBRPartitionTable(mbr)
}

func getMBRPartitionTable(mbr []byte) ([]PartitionInfo, error) {
	var parts []PartitionInfo
	for i := 0; i < 4; i++ {
		e := mbr[446+i*16 : 446+i*16+16]
		if e[4] == 0 {
			continue
		}
		lbaStart := uint64(binary.LittleEndian.Uint32(e[8:12]))
		lbaCnt := uint64(binary.LittleEndian.Uint32(e[12:16]))
		parts = append(parts, PartitionInfo{
			Num:       len(parts) + 1,
			StartByte: lbaStart * 512,
			SizeBytes: lbaCnt * 512,
		})
	}
	return parts, nil
}

func getGPTPartitionTable(chain []*QCOWLayer) ([]PartitionInfo, error) {
	hdr, err := ReadVirtualBytes(chain, 512, 512)
	if err != nil {
		return nil, err
	}
	if string(hdr[0:8]) != "EFI PART" {
		return nil, fmt.Errorf("invalid GPT signature")
	}
	entryLBA := binary.LittleEndian.Uint64(hdr[72:80])
	numEntries := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])
	entryBytes, err := ReadVirtualBytes(chain, entryLBA*512, uint64(numEntries)*uint64(entrySize))
	if err != nil {
		return nil, err
	}
	var parts []PartitionInfo
	for i := uint32(0); i < numEntries; i++ {
		e := entryBytes[i*entrySize : (i+1)*entrySize]
		allZero := true
		for _, b := range e[0:16] {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			continue
		}
		startLBA := binary.LittleEndian.Uint64(e[32:40])
		endLBA := binary.LittleEndian.Uint64(e[40:48])
		parts = append(parts, PartitionInfo{
			Num:       len(parts) + 1,
			StartByte: startLBA * 512,
			SizeBytes: (endLBA - startLBA + 1) * 512,
		})
	}
	return parts, nil
}

// WritePartitions writes each partition specified in targets from the virtual disk to its device.
func WritePartitions(chain []*QCOWLayer, targets []PartitionTarget) error {
	parts, err := GetPartitionTable(chain)
	if err != nil {
		return err
	}

	for _, target := range targets {
		var info *PartitionInfo
		for i := range parts {
			if parts[i].Num == target.Num {
				info = &parts[i]
				break
			}
		}
		if info == nil {
			return fmt.Errorf("partition %d not found in image", target.Num)
		}

		dst, err := os.OpenFile(target.Device, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return fmt.Errorf("open %s: %v", target.Device, err)
		}

		fmt.Printf("writing partition %d -> %s (%d bytes)\n", target.Num, target.Device, info.SizeBytes)
		const chunkSize = uint64(4 << 20)
		written := uint64(0)
		for written < info.SizeBytes {
			toRead := chunkSize
			if remaining := info.SizeBytes - written; toRead > remaining {
				toRead = remaining
			}
			data, err := ReadVirtualBytes(chain, info.StartByte+written, toRead)
			if err != nil {
				dst.Close()
				return fmt.Errorf("partition %d read at +%d: %v", target.Num, written, err)
			}
			if _, err := dst.WriteAt(data, int64(written)); err != nil {
				dst.Close()
				return fmt.Errorf("partition %d write to %s: %v", target.Num, target.Device, err)
			}
			written += toRead
			pct := written * 100 / info.SizeBytes
			filled := int(pct * 50 / 100)
			fmt.Printf("\r[%s%s] %3d%%",
				strings.Repeat(".", filled),
				strings.Repeat(" ", 50-filled),
				pct)
		}
		fmt.Println()
		dst.Close()
	}
	return nil
}

// ListPartitions prints the MBR or GPT partition table from the virtual disk.
func ListPartitions(chain []*QCOWLayer) error {
	mbr, err := ReadVirtualBytes(chain, 0, 512)
	if err != nil {
		return fmt.Errorf("reading MBR: %v", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xAA {
		return fmt.Errorf("no MBR signature at byte 510")
	}

	// Detect GPT: protective MBR has a partition with type 0xEE.
	for i := 0; i < 4; i++ {
		if mbr[446+i*16+4] == 0xEE {
			return listGPTPartitions(chain)
		}
	}
	return listMBRPartitions(mbr)
}

func listMBRPartitions(mbr []byte) error {
	fmt.Printf("%-4s  %-18s  %10s  %16s\n", "#", "Type", "LBA Start", "Size (bytes)")
	found := 0
	for i := 0; i < 4; i++ {
		e := mbr[446+i*16 : 446+i*16+16]
		ptype := e[4]
		if ptype == 0 {
			continue
		}
		lbaStart := binary.LittleEndian.Uint32(e[8:12])
		lbaCnt := binary.LittleEndian.Uint32(e[12:16])
		fmt.Printf("%-4d  %-18s  %10d  %16d\n", i+1, mbrTypeName(ptype), lbaStart, uint64(lbaCnt)*512)
		found++
	}
	if found == 0 {
		fmt.Println("no partitions found")
	}
	return nil
}

func listGPTPartitions(chain []*QCOWLayer) error {
	hdr, err := ReadVirtualBytes(chain, 512, 512)
	if err != nil {
		return fmt.Errorf("reading GPT header: %v", err)
	}
	if string(hdr[0:8]) != "EFI PART" {
		return fmt.Errorf("invalid GPT signature")
	}

	entryLBA := binary.LittleEndian.Uint64(hdr[72:80])
	numEntries := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])

	entryBytes, err := ReadVirtualBytes(chain, entryLBA*512, uint64(numEntries)*uint64(entrySize))
	if err != nil {
		return fmt.Errorf("reading GPT entries: %v", err)
	}

	fmt.Printf("%-4s  %-30s  %10s  %16s  %s\n", "#", "Type", "LBA Start", "Size (bytes)", "Name")
	found := 0
	for i := uint32(0); i < numEntries; i++ {
		e := entryBytes[i*entrySize : (i+1)*entrySize]
		// Empty entry: type GUID is all zeros.
		allZero := true
		for _, b := range e[0:16] {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			continue
		}
		guid := formatGUID(e[0:16])
		startLBA := binary.LittleEndian.Uint64(e[32:40])
		endLBA := binary.LittleEndian.Uint64(e[40:48])
		size := (endLBA - startLBA + 1) * 512
		name := decodeUTF16LE(e[56:128])
		fmt.Printf("%-4d  %-30s  %10d  %16d  %s\n", found+1, gptTypeName(guid), startLBA, size, name)
		found++
	}
	if found == 0 {
		fmt.Println("no partitions found")
	}
	return nil
}

func mbrTypeName(t byte) string {
	switch t {
	case 0x01:
		return "FAT12"
	case 0x04:
		return "FAT16 <32M"
	case 0x05:
		return "Extended"
	case 0x06:
		return "FAT16"
	case 0x07:
		return "NTFS/exFAT"
	case 0x0b:
		return "FAT32"
	case 0x0c:
		return "FAT32 LBA"
	case 0x0e:
		return "FAT16 LBA"
	case 0x0f:
		return "Extended LBA"
	case 0x82:
		return "Linux swap"
	case 0x83:
		return "Linux"
	case 0x8e:
		return "Linux LVM"
	case 0xa5:
		return "FreeBSD"
	case 0xaf:
		return "macOS HFS+"
	case 0xee:
		return "GPT protective"
	case 0xef:
		return "EFI System"
	case 0xfd:
		return "Linux RAID"
	default:
		return fmt.Sprintf("0x%02x", t)
	}
}

func formatGUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.LittleEndian.Uint32(b[0:4]),
		binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

func gptTypeName(guid string) string {
	switch guid {
	case "c12a7328-f81f-11d2-ba4b-00a0c93ec93b":
		return "EFI System"
	case "21686148-6449-6e6f-744e-656564454649":
		return "BIOS Boot"
	case "0fc63daf-8483-4772-8e79-3d69d8477de4":
		return "Linux filesystem"
	case "e6d6d379-f507-44c2-a23c-238f2a3df928":
		return "Linux LVM"
	case "a19d880f-05fc-4d3b-a006-743f0f84911e":
		return "Linux RAID"
	case "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f":
		return "Linux swap"
	case "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7":
		return "Windows data"
	case "de94bba4-06d1-4d40-a16a-bfd50179d6ac":
		return "Windows Recovery"
	case "48465300-0000-11aa-aa11-00306543ecac":
		return "macOS HFS+"
	case "426f6f74-0000-11aa-aa11-00306543ecac":
		return "macOS Boot"
	default:
		return guid
	}
}

func decodeUTF16LE(b []byte) string {
	var out []rune
	for i := 0; i+1 < len(b); i += 2 {
		r := rune(binary.LittleEndian.Uint16(b[i : i+2]))
		if r == 0 {
			break
		}
		out = append(out, r)
	}
	return string(out)
}
