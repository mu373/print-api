GO := go
GO_TOOLCHAIN := go1.25.3
GOOS := linux
GOARCH := amd64
CGO_ENABLED := 0

BINARY := dist/print-api
BUILD_FLAGS := -trimpath -buildvcs=false -mod=readonly
LDFLAGS := -s -w -buildid=

.PHONY: all build check test generate clean checksum

all: check build

build:
	@mkdir -p $(dir $(BINARY))
	GOTOOLCHAIN=$(GO_TOOLCHAIN) CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build $(BUILD_FLAGS) -ldflags='$(LDFLAGS)' -o $(BINARY) .

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt is required for:"; gofmt -l .; exit 1; }
	GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) vet -mod=readonly ./...
	GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) test -mod=readonly ./...

test:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) test -mod=readonly ./...

generate:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) generate ./...
	GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) mod tidy

checksum: build
	sha256sum $(BINARY)

clean:
	rm -f $(BINARY)
