package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/baremetal-labs/qflash"
)

func main() {
	srcFile := flag.String("src", "", "Source qcow2 file")
	dstFile := flag.String("dst", "", "Destination device or raw file to write to")
	listParts := flag.Bool("list-parts", false, "List partition table from virtual disk")
	writeParts := flag.String("write-parts", "", `Write individual partitions, e.g. "1=/dev/sda1,2=/dev/sda2"`)
	debug := flag.Bool("debug", false, "Print QCOW2 internal details and diagnostics")
	flag.Parse()

	if *srcFile == "" {
		fmt.Fprintln(os.Stderr, "usage: qflash -src <file.qcow2> -dst <device> [-debug]")
		os.Exit(1)
	}
	qflash.DebugMode = *debug

	src, err := os.Open(*srcFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qflash: open source: %v\n", err)
		os.Exit(1)
	}
	defer src.Close()

	chain, err := qflash.OpenLayerChain(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qflash: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		for _, l := range chain[1:] {
			_ = l.File.Close()
		}
	}()

	header := chain[0].Header

	if qflash.DebugMode {
		fileInfo, err := src.Stat()
		if err != nil {
			fmt.Fprintf(os.Stderr, "qflash: stat: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("=== QCOW2 File: %s ===\n", *srcFile)
		fmt.Printf("  Version:           %d\n", header.Version)
		fmt.Printf("  Image file size:   %d bytes\n", fileInfo.Size())
		fmt.Printf("  Virtual disk size: %d bytes\n", header.Size)
		fmt.Printf("  Cluster size:      %d bytes (bits=%d)\n", header.ClusterSize, header.ClusterBits)
		fmt.Printf("  L1 entries:        %d at offset 0x%x\n", header.L1Size, header.L1TableOffset)
		if len(chain) > 1 {
			fmt.Printf("  Backing chain:     %d layer(s)\n", len(chain))
		}
		if header.Version >= 3 {
			fmt.Printf("  Incompat flags:    0x%x\n", header.IncompatibleFeatures)
			fmt.Printf("  Compat flags:      0x%x\n", header.CompatibleFeatures)
		}
	}

	if *listParts {
		if err := qflash.ListPartitions(chain); err != nil {
			fmt.Fprintf(os.Stderr, "qflash: list-parts: %v\n", err)
			os.Exit(1)
		}
	}

	if *writeParts != "" {
		var targets []qflash.PartitionTarget
		for _, tok := range strings.Split(*writeParts, ",") {
			eq := strings.IndexByte(tok, '=')
			if eq < 1 {
				fmt.Fprintf(os.Stderr, "qflash: write-parts: invalid token %q (want num=device)\n", tok)
				os.Exit(1)
			}
			var num int
			if _, err := fmt.Sscanf(tok[:eq], "%d", &num); err != nil {
				fmt.Fprintf(os.Stderr, "qflash: write-parts: invalid partition number %q\n", tok[:eq])
				os.Exit(1)
			}
			targets = append(targets, qflash.PartitionTarget{Num: num, Device: tok[eq+1:]})
		}
		if err := qflash.WritePartitions(chain, targets); err != nil {
			fmt.Fprintf(os.Stderr, "qflash: write-parts: %v\n", err)
			os.Exit(1)
		}
	}

	if *dstFile != "" {
		dst, err := os.OpenFile(*dstFile, os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qflash: open destination: %v\n", err)
			os.Exit(1)
		}
		defer dst.Close()

		fmt.Printf("qflash: %s -> %s (%s)\n", *srcFile, *dstFile, qflash.HumanSize(header.Size))
		if err := qflash.WriteToDevice(chain, dst); err != nil {
			fmt.Fprintf(os.Stderr, "qflash: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("qflash: done")
	}
}
