APP := cm
VERSION ?= dev
GOCACHE ?= $(CURDIR)/.cache/go-build

.PHONY: test build deb deb-all clean

test:
	node scripts/check-web-js.mjs
	GOCACHE=$(GOCACHE) go test ./...

build:
	GOCACHE=$(GOCACHE) go build -ldflags "-X main.version=$(VERSION)" -o bin/$(APP) ./cmd/$(APP)

deb:
	GOCACHE=$(GOCACHE) VERSION=$(VERSION) ARCH=$(ARCH) scripts/build-deb.sh

deb-all:
	GOCACHE=$(GOCACHE) VERSION=$(VERSION) scripts/build-deb.sh --all

clean:
	rm -rf bin dist .cache
