BINARY := dsh-remote
PKG := ./cmd/dsh-remote
VERSION ?= 0.1.0

.PHONY: build test vet fmt clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -f $(BINARY)
