VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Mayuresh1m2/PersonalDrive/server/internal/app.Version=$(VERSION)
GO ?= go

.PHONY: build build-all test test-race vet fmt demo-phase1 drill clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/pdrive ./cmd/pdrive

# Cross-compile for the laptop (amd64) and a future Pi / ARM mini PC (arm64).
build-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/pdrive-linux-amd64 ./cmd/pdrive
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/pdrive-linux-arm64 ./cmd/pdrive

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

fmt:
	gofmt -w .

demo-phase1: build
	../scripts/demo-phase1.sh

# Lose-everything drill: back up, delete the machine, rebuild with bootstrap.sh.
drill:
	../scripts/disaster-drill.sh

clean:
	rm -rf bin
