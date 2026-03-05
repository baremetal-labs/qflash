package qflash

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WriteToDevice streams the virtual disk contents through the layer chain to
// dst using a pool of parallel workers — one per logical CPU. ReadAt and
// WriteAt are safe for concurrent use (pread/pwrite), and decompression is
// CPU-bound, so the worker pool gives near-linear throughput scaling.
func WriteToDevice(chain []*QCOWLayer, dst *os.File) error {
	top := chain[0]
	clusterSize := top.Header.ClusterSize
	totalClusters := (top.Header.Size + clusterSize - 1) / clusterSize

	if DebugMode {
		fmt.Printf("Virtual disk size: %d bytes (%d clusters of %d bytes)\n",
			top.Header.Size, totalClusters, clusterSize)
		if len(chain) > 1 {
			fmt.Printf("Backing chain depth: %d\n", len(chain))
		}
	}

	numWorkers := runtime.NumCPU()
	jobs := make(chan uint64, numWorkers*4)

	var (
		mu       sync.Mutex
		firstErr error
	)
	var written, zeros, completed atomic.Int64

	// Progress bar: updates in place using \r, 50 chars wide.
	printProgress := func(done int64) {
		pct := done * 100 / int64(totalClusters)
		filled := int(pct * 50 / 100)
		fmt.Printf("\r[%s%s] %3d%%",
			strings.Repeat(".", filled),
			strings.Repeat(" ", 50-filled),
			pct)
	}
	stop := make(chan struct{})
	var progressWg sync.WaitGroup
	progressWg.Add(1)
	go func() {
		defer progressWg.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				printProgress(completed.Load())
			case <-stop:
				printProgress(completed.Load())
				fmt.Println()
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for clusterNum := range jobs {
				virtualOffset := clusterNum * clusterSize

				data, err := readVirtualClusterChain(chain, virtualOffset)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("cluster %d (offset 0x%x): %v", clusterNum, virtualOffset, err)
					}
					mu.Unlock()
					completed.Add(1)
					continue
				}

				// Trim the last cluster to the actual virtual disk size.
				writeLen := uint64(len(data))
				if remaining := top.Header.Size - virtualOffset; remaining < writeLen {
					writeLen = remaining
				}

				if _, err = dst.WriteAt(data[:writeLen], int64(virtualOffset)); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("error writing cluster %d: %v", clusterNum, err)
					}
					mu.Unlock()
					completed.Add(1)
					continue
				}

				if isZeroBuf(data[:writeLen]) {
					zeros.Add(1)
				} else {
					written.Add(1)
				}
				completed.Add(1)
			}
		}()
	}

	for clusterNum := uint64(0); clusterNum < totalClusters; clusterNum++ {
		mu.Lock()
		e := firstErr
		mu.Unlock()
		if e != nil {
			break
		}
		jobs <- clusterNum
	}
	close(jobs)
	wg.Wait()
	close(stop)
	progressWg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if DebugMode {
		fmt.Printf("written: %d data clusters, %d zero clusters\n", written.Load(), zeros.Load())
	}
	return nil
}
