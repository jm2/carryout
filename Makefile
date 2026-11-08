# carryout stamps the bare version (carryout 0.2.0), not the tag. Outside a
# git checkout VERSION is empty and the binary keeps main.go's dev version.
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null))
LDFLAGS := -s -w $(if $(VERSION),-X main.version=$(VERSION))
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build test vet fmt-check cross bindir deb clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o carryout .

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

test: vet
	go test -race ./...

# dist/carryout-<os>-<arch>[.exe]
cross:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building carryout $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/carryout-$$os-$$arch$$ext . || exit 1; \
	done

# dist/linux-<arch>/carryout: the layout the package scripts take.
ARCH ?= amd64
bindir:
	@test -f dist/carryout-linux-$(ARCH) || { echo "run make cross first"; exit 1; }
	@mkdir -p dist/linux-$(ARCH)
	cp dist/carryout-linux-$(ARCH) dist/linux-$(ARCH)/carryout

# A .deb for ARCH (amd64 or arm64) from the cross-built binary.
deb: bindir
	build-aux/linux/build-deb.sh "$(VERSION)" $(ARCH) dist/linux-$(ARCH) dist/carryout-$(ARCH).deb

clean:
	rm -rf carryout dist
