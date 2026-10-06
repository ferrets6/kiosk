# SPDX-License-Identifier: MIT

GO ?= go
VERSION ?= dev

.PHONY: build test vet fmt check image clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/kiosk ./cmd/kiosk

# What CI gates on: unit and contract tests, with the race detector.
test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

check: vet test
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt clean:"; echo "$$unformatted"; exit 1; fi

image:
	docker build --build-arg VERSION=$(VERSION) -t kiosk:$(VERSION) .

clean:
	rm -rf bin

