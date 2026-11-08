package mgmt

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Event is an asynchronous mgmt event.
type Event struct {
	Code   uint16
	Index  uint16
	Params []byte
}

// ErrClosed is returned by Do after the connection ended.
var ErrClosed = errors.New("mgmt: connection closed")

// CommandTimeout bounds how long Do waits for the controller's answer.
var CommandTimeout = 5 * time.Second

type reply struct {
	st   StatusCode
	data []byte
}

type pending struct {
	op, index uint16
	ch        chan reply
}

// Client speaks the mgmt protocol over a packet-oriented connection (the mgmt
// socket returns one packet per Read). Commands are serialized; events are
// delivered on Events().
type Client struct {
	rw     io.ReadWriteCloser
	log    *slog.Logger
	events chan Event
	done   chan struct{}
	quit   chan struct{} // closed by Close; stops the event forwarder
	once   sync.Once
	rwOnce sync.Once

	// Event queue between readLoop and forward, so readLoop never blocks on a
	// slow consumer and keeps delivering command results to Do.
	qmu   sync.Mutex
	q     []Event
	ended bool          // readLoop exited; forward closes events once q drains
	wake  chan struct{} // cap 1

	cmdMu sync.Mutex // serializes Do
	mu    sync.Mutex
	pend  *pending
	err   error
}

// NewClient starts the reader goroutine. The client owns rw and closes it when
// the connection ends. Ordinary events are dropped (and logged) if the consumer
// falls more than maxQueued events behind; pairing/key events are queued up to
// maxCritical (a flood far beyond anything the kernel sends).
func NewClient(rw io.ReadWriteCloser, log *slog.Logger) *Client {
	c := &Client{rw: rw, log: log, events: make(chan Event), done: make(chan struct{}),
		quit: make(chan struct{}), wake: make(chan struct{}, 1)}
	go c.readLoop()
	go c.forward()
	return c
}

const (
	maxQueued   = 256
	maxCritical = 4096
)

// enqueue hands ev to forward without ever blocking readLoop.
func (c *Client) enqueue(ev Event, critical bool) {
	c.qmu.Lock()
	n := len(c.q)
	ok := n < maxQueued || critical && n < maxCritical
	if ok {
		c.q = append(c.q, ev)
	}
	c.qmu.Unlock()
	if !ok {
		c.log.Warn("mgmt: event queue full, dropping", "code", ev.Code)
		return
	}
	c.signal()
}

func (c *Client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// forward delivers queued events in order on Events(), and closes it after the
// reader ended and the queue drained, or on Close.
func (c *Client) forward() {
	defer close(c.events)
	for {
		c.qmu.Lock()
		for len(c.q) == 0 {
			ended := c.ended
			c.qmu.Unlock()
			if ended {
				return
			}
			select {
			case <-c.wake:
			case <-c.quit:
				return
			}
			c.qmu.Lock()
		}
		ev := c.q[0]
		c.q[0] = Event{}
		c.q = c.q[1:]
		c.qmu.Unlock()
		select {
		case c.events <- ev:
		case <-c.quit:
			return
		}
	}
}

// Events delivers asynchronous events; it is closed when the connection ends
// (after the queued events were delivered) or on Close.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the connection ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close closes the underlying connection and stops event delivery.
// It is idempotent.
func (c *Client) Close() error {
	c.once.Do(func() { close(c.quit) })
	return c.closeRW()
}

func (c *Client) closeRW() error {
	var err error
	c.rwOnce.Do(func() { err = c.rw.Close() })
	return err
}

func (c *Client) readLoop() {
	defer c.closeRW()
	defer close(c.done)
	defer func() {
		c.qmu.Lock()
		c.ended = true
		c.qmu.Unlock()
		c.signal()
	}()
	buf := make([]byte, 4096)
	for {
		n, err := c.rw.Read(buf)
		if err != nil || n == 0 {
			if err == nil {
				err = io.EOF
			}
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		h, params, err := DecodePacket(buf[:n])
		if err != nil {
			c.log.Warn("mgmt: bad packet", "err", err)
			continue
		}
		params = append([]byte(nil), params...)
		switch h.Code {
		case EvCmdComplete, EvCmdStatus:
			op, st, data, err := DecodeCmdResult(params)
			if err != nil {
				c.log.Warn("mgmt: bad command result", "err", err)
				continue
			}
			c.mu.Lock()
			p := c.pend
			if p != nil && p.op == op && p.index == h.Index {
				c.pend = nil
				c.mu.Unlock()
				// Command Status with success is only an ack for commands that
				// complete later; none of ours do, so treat both as the result.
				p.ch <- reply{st, data}
				continue
			}
			c.mu.Unlock()
			c.log.Debug("mgmt: unmatched command result", "op", op, "status", st)
		default:
			// Pairing/key events must not be lost (a dropped key means a re-pair).
			crit := h.Code == EvNewLinkKey || h.Code == EvUserConfirmReq || h.Code == EvPINCodeRequest
			c.enqueue(Event{h.Code, h.Index, params}, crit)
		}
	}
}

// Do sends a command and waits for its Command Complete/Status. A non-zero
// status yields a *StatusError (data is still returned). May be called from the
// goroutine that drains Events().
func (c *Client) Do(index, opcode uint16, params []byte) ([]byte, error) {
	if len(params) > 0xffff {
		return nil, fmt.Errorf("mgmt command 0x%04x: %d bytes of parameters, max 65535", opcode, len(params))
	}
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	p := &pending{op: opcode, index: index, ch: make(chan reply, 1)}
	c.mu.Lock()
	if c.err != nil || c.isDone() {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pend = p
	c.mu.Unlock()
	if _, err := c.rw.Write(EncodePacket(opcode, index, params)); err != nil {
		c.clear(p)
		return nil, err
	}
	t := time.NewTimer(CommandTimeout)
	defer t.Stop()
	select {
	case r := <-p.ch:
		if r.st != 0 {
			return r.data, &StatusError{Op: opcode, Status: r.st}
		}
		return r.data, nil
	case <-c.done:
		return nil, ErrClosed
	case <-t.C:
		c.clear(p)
		return nil, errors.New("mgmt: command timeout")
	}
}

func (c *Client) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Client) clear(p *pending) {
	c.mu.Lock()
	if c.pend == p {
		c.pend = nil
	}
	c.mu.Unlock()
}
