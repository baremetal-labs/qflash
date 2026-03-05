BINARY := qflash

.PHONY: all build test clean

all: build

test:
	go test -v ./...

build: \
	dist/$(BINARY)-darwin-arm64 \
	dist/$(BINARY)-linux-amd64

dist/$(BINARY)-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o $@ ./cmd/qflash

dist/$(BINARY)-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o $@ ./cmd/qflash

clean:
	rm -rf dist/
