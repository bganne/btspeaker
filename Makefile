.PHONY: build arm test vet bench-arm

build:
	go build -o btspeaker .

GOARM ?= 7

arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=$(GOARM) go build -trimpath -ldflags='-s -w' -o btspeaker-armv7 .

test:
	go test ./...

vet:
	go vet ./...
	GOARCH=arm go vet ./...

# Cross-compiled test binaries to run on the cubox: ./sbc.test -test.bench=. (see bench-arm/)
bench-arm:
	mkdir -p bench-arm
	GOARCH=arm GOARM=$(GOARM) CGO_ENABLED=0 go test -c -o bench-arm/ ./internal/sbc ./internal/player
