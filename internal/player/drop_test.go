package player

import (
	"errors"
	"sync"
	"testing"
	"time"

	"btspeaker/internal/alsa"
)

// hangPCM blocks in Write until Abort, like an S/PDIF device without a clock.
type hangPCM struct {
	fakePCM
	once    sync.Once
	release chan struct{}
	started chan struct{}
}

func (h *hangPCM) Write(b []byte) error {
	h.once.Do(func() { close(h.started) })
	<-h.release
	return errors.New("aborted")
}
func (h *hangPCM) Abort() error {
	h.fakePCM.Abort()
	select {
	case <-h.release:
	default:
		close(h.release)
	}
	return nil
}

type hangBackend struct {
	fakeBackend
	h *hangPCM
}

func (b *hangBackend) Open(string, int, int, alsa.Format, int) (PCM, error) { return b.h, nil }

func TestDropUnwedgesHungWrite(t *testing.T) {
	h := &hangPCM{release: make(chan struct{}), started: make(chan struct{})}
	b := &hangBackend{fakeBackend: fakeBackend{caps: stdCaps()}, h: h}
	p := NewWithBackend(Config{}, b)
	p.Start(44100, 2)
	p.Write(loud)
	<-h.started
	start := time.Now()
	p.Drop()
	if time.Since(start) > time.Second {
		t.Fatal("Drop waited for the hung write")
	}
	if !h.isClosed() || p.Status().Open {
		t.Fatal("device not released")
	}
}

func TestStopDrainsDropDoesNot(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	p.Stop()
	if pc := b.pcm(0); !pc.drained || pc.aborted {
		t.Fatal("Stop must drain, not abort")
	}
	p.Start(44100, 2)
	p.Write(loud)
	p.Drop()
	if pc := b.pcm(1); pc.drained || !pc.aborted || !pc.isClosed() {
		t.Fatalf("Drop must abort without drain: %+v", pc)
	}
}
