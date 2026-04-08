package qflash

import (
	"fmt"
	"io"
	"net/http"
	"sync"
)

const defaultChunkSize = 1 << 20 // 1 MiB

// chunkResult holds the result of a single chunk fetch, shared across
// concurrent callers requesting the same chunk.
type chunkResult struct {
	done chan struct{}
	data []byte
	err  error
}

// HTTPReaderAt implements io.ReaderAt against a remote URL using HTTP range
// requests. Fetched chunks are cached in memory to avoid redundant requests
// when the same region is read multiple times (e.g. repeated L2 table lookups).
type HTTPReaderAt struct {
	url       string
	client    *http.Client
	chunkSize int64
	mu        sync.Mutex
	cache     map[int64]*chunkResult // key: chunk index (offset / chunkSize)
}

// NewHTTPReaderAt creates an HTTPReaderAt for the given URL using the default
// HTTP client and a 1 MiB chunk size.
func NewHTTPReaderAt(url string) *HTTPReaderAt {
	return &HTTPReaderAt{
		url:       url,
		client:    http.DefaultClient,
		chunkSize: defaultChunkSize,
		cache:     make(map[int64]*chunkResult),
	}
}

// NewHTTPReaderAtWithClient creates an HTTPReaderAt with a custom HTTP client
// and chunk size.
func NewHTTPReaderAtWithClient(url string, client *http.Client, chunkSize int64) *HTTPReaderAt {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	return &HTTPReaderAt{
		url:       url,
		client:    client,
		chunkSize: chunkSize,
		cache:     make(map[int64]*chunkResult),
	}
}

// ReadAt implements io.ReaderAt. It fetches the minimal set of chunks needed
// to satisfy the request, caching each chunk for future reads.
func (h *HTTPReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	total := 0
	for total < len(p) {
		chunkIdx := (off + int64(total)) / h.chunkSize
		chunkOff := (off + int64(total)) % h.chunkSize

		chunk, err := h.getChunk(chunkIdx)
		if err != nil {
			return total, err
		}

		if chunkOff >= int64(len(chunk)) {
			return total, io.EOF
		}

		n := copy(p[total:], chunk[chunkOff:])
		total += n
	}

	return total, nil
}

// getChunk returns the cached chunk at chunkIdx, fetching it via HTTP range
// request if not already cached. Concurrent requests for the same chunk
// share a single HTTP fetch.
func (h *HTTPReaderAt) getChunk(chunkIdx int64) ([]byte, error) {
	h.mu.Lock()
	res, ok := h.cache[chunkIdx]
	if ok {
		h.mu.Unlock()
		<-res.done
		return res.data, res.err
	}
	res = &chunkResult{done: make(chan struct{})}
	h.cache[chunkIdx] = res
	h.mu.Unlock()

	res.data, res.err = h.fetchChunk(chunkIdx)
	close(res.done)

	return res.data, res.err
}

// fetchChunk issues a single HTTP range request for the given chunk.
func (h *HTTPReaderAt) fetchChunk(chunkIdx int64) ([]byte, error) {
	start := chunkIdx * h.chunkSize
	end := start + h.chunkSize - 1

	req, err := http.NewRequest(http.MethodGet, h.url, nil)
	if err != nil {
		return nil, fmt.Errorf("http range request: %v", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http range request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http range request: unexpected status %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http range request: reading body: %v", err)
	}

	return data, nil
}
