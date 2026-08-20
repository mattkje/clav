BINARY  := clav
# The version comes from the git tag, without its leading "v".
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
LDFLAGS := -s -w -X github.com/mattkje/clav/internal/cli.Version=$(VERSION)
PREFIX  ?= /usr/local

.PHONY: all build test race vet fmt install uninstall clean dist checksums

all: vet test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/clav

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

clean:
	rm -rf bin dist

# Release binaries for the supported platforms.
dist: clean
	@mkdir -p dist
	@for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/$(BINARY)-$$os-$$arch ./cmd/clav || exit 1; \
	done
	@ls -1 dist

# checksums.txt is what install.sh verifies the download against.
checksums: 
	@cd dist && (sha256sum * 2>/dev/null || shasum -a 256 *) | grep -v checksums.txt > checksums.txt
	@cat dist/checksums.txt
