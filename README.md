# qflash

Flash a qcow2 disk image directly to a raw block device or file.

## Build

```sh
make                        # builds both targets
make clean                  # removes dist/
```

Output binaries:

| Binary | Platform |
|--------|----------|
| `dist/qflash-darwin-arm64` | macOS (Apple Silicon) |
| `dist/qflash-linux-amd64`  | Linux (x86-64) |

## Usage

```sh
# Inspect image headers
qflash -src image.qcow2

# Write virtual disk contents to a block device (dd-style)
qflash -src image.qcow2 -dst /dev/sdX

# Write to a raw file instead
qflash -src image.qcow2 -dst output.raw

# List partitions in the virtual disk
qflash -src image.qcow2 -list-parts

# Write individual partitions to separate devices
qflash -src image.qcow2 -write-parts 1=/dev/sda1,2=/dev/sda2
```

## Flags

| Flag | Description |
|------|-------------|
| `-src` | Path to the source qcow2 image (required) |
| `-dst` | Destination device or raw file to write to (optional) |
| `-list-parts` | List partition table from the virtual disk |
| `-write-parts` | Write individual partitions, e.g. `1=/dev/sda1,2=/dev/sda2` |
| `-debug` | Print QCOW2 internal details and diagnostics |

## Docs

The `docs/` directory contains reference material used to guide the implementation of the qcow2 library functions.

## How it works

qflash implements the qcow2 format natively in Go. For each virtual cluster it:

1. Walks the two-level L1/L2 table to find the host cluster location
2. Reads the cluster (decompressing if needed — raw deflate)
3. Writes it to the destination at the corresponding virtual offset

Unallocated clusters are skipped; those bytes on the destination device are left unchanged (sparse write).

Supports qcow2 version 2 and 3, standard clusters, zero-flag clusters, and deflate-compressed clusters.
