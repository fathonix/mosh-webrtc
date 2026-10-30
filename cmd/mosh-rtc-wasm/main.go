//go:build js && wasm

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"syscall/js"
	"time"

	mosh "github.com/unixshells/mosh-go"
)

func main() {
	js.Global().Set("moshConnectRTC", js.FuncOf(moshConnectRTC))
	select {} // keep alive in browser runtime
}

// moshConnectRTC(rtcDataChannel, key, cols, rows) -> Promise<Session>
func moshConnectRTC(this js.Value, args []js.Value) interface{} {
	if len(args) < 4 {
		return reject("moshConnectRTC requires (dataChannel, key, cols, rows)")
	}

	dc := args[0]
	key := args[1].String()
	cols := args[2].Int()
	rows := args[3].Int()

	handler := js.FuncOf(func(this js.Value, pargs []js.Value) interface{} {
		resolve := pargs[0]
		rejectFn := pargs[1]

		go func() {
			session, err := newRTCSession(dc, key, cols, rows)
			if err != nil {
				rejectFn.Invoke(err.Error())
				return
			}
			resolve.Invoke(session.jsObject())
		}()

		return nil
	})

	return js.Global().Get("Promise").New(handler)
}

type rtcConn struct {
	mu       sync.Mutex
	dc       js.Value
	incoming chan []byte
	done     chan struct{}
	deadline time.Time
	onMsg    js.Func
}

func newRTCConn(dc js.Value) *rtcConn {
	c := &rtcConn{
		dc:       dc,
		incoming: make(chan []byte, 512),
		done:     make(chan struct{}),
	}

	c.attachDataChannel(dc)
	return c
}

func (c *rtcConn) attachDataChannel(dc js.Value) {
	c.onMsg = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) == 0 {
			return nil
		}
		data := args[0].Get("data")
		if data.InstanceOf(js.Global().Get("ArrayBuffer")) {
			u8 := js.Global().Get("Uint8Array").New(data)
			buf := make([]byte, u8.Length())
			js.CopyBytesToGo(buf, u8)
			select {
			case c.incoming <- buf:
			default:
			}
		}
		return nil
	})

	dc.Set("binaryType", "arraybuffer")
	dc.Call("addEventListener", "message", c.onMsg)
	c.dc = dc
}

// swapDataChannel replaces the underlying WebRTC DataChannel (e.g. after ICE restart or reconnect)
func (c *rtcConn) swapDataChannel(newDC js.Value) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.dc.IsNull() && !c.dc.IsUndefined() {
		c.dc.Call("removeEventListener", "message", c.onMsg)
		c.onMsg.Release()
	}

	c.attachDataChannel(newDC)
}

func (c *rtcConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if !dl.IsZero() {
		dur := time.Until(dl)
		if dur <= 0 {
			return 0, errors.New("i/o timeout")
		}
		timer = time.NewTimer(dur)
		timeoutCh = timer.C
		defer timer.Stop()
	}

	select {
	case <-c.done:
		return 0, errors.New("connection closed")
	case <-timeoutCh:
		return 0, errors.New("i/o timeout")
	case pkt := <-c.incoming:
		n := copy(b, pkt)
		return n, nil
	}
}

func (c *rtcConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	dc := c.dc
	c.mu.Unlock()

	if dc.IsNull() || dc.IsUndefined() {
		return len(b), nil
	}

	// ReadyState: "open" is 1
	if readyState := dc.Get("readyState").String(); readyState != "open" {
		return len(b), nil
	}

	u8 := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(u8, b)
	dc.Call("send", u8)
	return len(b), nil
}

func (c *rtcConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *rtcConn) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.mu.Lock()
	if !c.dc.IsNull() && !c.dc.IsUndefined() {
		c.dc.Call("removeEventListener", "message", c.onMsg)
		c.onMsg.Release()
	}
	c.mu.Unlock()
	return nil
}

type rtcSession struct {
	client  *mosh.Client
	conn    *rtcConn
	tracker *stateTracker
	mu      sync.Mutex
	closed  bool
}

func newRTCSession(dc js.Value, key string, cols, rows int) (*rtcSession, error) {
	for len(key)%4 != 0 {
		key += "="
	}
	rawKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("bad mosh key: %w", err)
	}
	ocb, err := mosh.NewOCB(rawKey)
	if err != nil {
		return nil, err
	}

	conn := newRTCConn(dc)
	client, err := mosh.DialConnRaw(conn, ocb)
	if err != nil {
		conn.Close()
		return nil, err
	}

	// Tell mosh-server our initial dimensions immediately
	if cols > 0 && rows > 0 {
		client.Resize(uint16(cols), uint16(rows))
		client.Tick()
	}

	s := &rtcSession{
		client:  client,
		conn:    conn,
		tracker: newStateTracker(cols, rows),
	}
	client.Transport().SetCanApply(s.tracker.has)

	go s.recvLoop()
	return s, nil
}

func (s *rtcSession) jsObject() js.Value {
	obj := js.Global().Get("Object").New()

	obj.Set("send", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 1 {
			return nil
		}
		data := []byte(args[0].String())
		s.client.Send(data)
		s.client.Tick()

		if out := s.tracker.keystroke(data); len(out) > 0 {
			return string(out)
		}
		return js.Null()
	}))

	obj.Set("resize", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 2 {
			return nil
		}
		cols := args[0].Int()
		rows := args[1].Int()
		s.client.Resize(uint16(cols), uint16(rows))
		s.tracker.resize(cols, rows)
		s.client.Tick()
		return nil
	}))

	obj.Set("swapDataChannel", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 1 {
			return nil
		}
		newDC := args[0]
		s.conn.swapDataChannel(newDC)
		// Force retransmission of current state over new channel
		s.client.Transport().ForceNextSend()
		s.client.Tick()
		return nil
	}))

	obj.Set("tick", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		s.client.Tick()
		return nil
	}))

	// Liveness for the client-side stall watchdog.
	obj.Set("sinceRecvMs", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		return float64(time.Since(s.client.Transport().LastRecv()).Milliseconds())
	}))

	obj.Set("unackedMs", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		return float64(s.client.Transport().UnackedDuration().Milliseconds())
	}))

	obj.Set("poll", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		out := s.tracker.poll()
		if out == nil {
			return js.Null()
		}
		return string(out)
	}))

	obj.Set("redraw", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		out := s.tracker.redraw()
		if out == nil {
			return js.Null()
		}
		return string(out)
	}))

	obj.Set("close", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.client.Close()
		s.conn.Close()
		return nil
	}))

	return obj
}

func (s *rtcSession) recvLoop() {
	for {
		diff := s.client.RecvRaw(1 * time.Second)
		if diff == nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		t := s.client.Transport()
		oldNum := t.LastRecvOldNum()
		newNum := t.LastRecvNewNum()
		throwawayNum := t.ThrowawayNum()
		if s.tracker.applyDiff(diff, oldNum, newNum, throwawayNum) {
			// Immediately send ACK back to server so the session state advances without delay
			t.AckApplied(newNum)
		}

		s.client.Tick()
	}
}

func reject(msg string) js.Value {
	return js.Global().Get("Promise").Call("reject", msg)
}
