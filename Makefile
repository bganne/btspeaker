.PHONY: build arm test test-race vet bench-arm

# CGO_ENABLED=0: dropping capabilities needs syscall.AllThreadsSyscall, which cgo disables.
build:
	CGO_ENABLED=0 go build -o btspeaker .

GOARM ?= 7

arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=$(GOARM) go build -trimpath -ldflags='-s -w' -o btspeaker-armv7 .

test: vet
	CGO_ENABLED=0 go test ./...

# -race needs cgo (gcc) on the host.
test-race: vet
	go test -race ./...

vet:
	go vet ./...
	GOARCH=arm go vet ./...

# Cross-compiled test binaries to run on the cubox: ./sbc.test -test.bench=. (see bench-arm/)
bench-arm:
	mkdir -p bench-arm
	GOARCH=arm GOARM=$(GOARM) CGO_ENABLED=0 go test -c -o bench-arm/ ./internal/sbc ./internal/player
