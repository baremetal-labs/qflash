package qflash


// ReadVirtualBytes reads length bytes from the virtual disk starting at offset,
// spanning cluster boundaries as needed.
func ReadVirtualBytes(chain []*QCOWLayer, offset, length uint64) ([]byte, error) {
	clusterSize := chain[0].Header.ClusterSize
	result := make([]byte, length)
	for pos := uint64(0); pos < length; {
		virtualOffset := offset + pos
		clusterBase := virtualOffset &^ (clusterSize - 1)
		intraOffset := virtualOffset - clusterBase
		data, err := readVirtualClusterChain(chain, clusterBase)
		if err != nil {
			return nil, err
		}
		toCopy := clusterSize - intraOffset
		if remaining := length - pos; toCopy > remaining {
			toCopy = remaining
		}
		copy(result[pos:], data[intraOffset:intraOffset+toCopy])
		pos += toCopy
	}
	return result, nil
}

