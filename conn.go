package tcpmux

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

type connState struct {
	conn net.Conn

	master  Map32
	streams Map32

	idx uint32

	exitRead chan bool

	newStreamCallback func(state *readState)
	ErrorCallback     func(error) bool

	timeout int64
	stopped bool
	sync.Mutex
}

// When something serious happened, we broadcast it to every stream and close the master conn
// TCP connections may have temporary errors, but here we treat them as the same as other failures
func (cs *connState) broadcast(err error) {
	if cs.ErrorCallback != nil {
		cs.ErrorCallback(err)
	}

	cs.streams.Iterate(func(idx uint32, s unsafe.Pointer) bool {
		(*Stream)(s).readResp <- &readState{err: err}
		return true
	})

	cs.stop()
}

func (cs *connState) start() {
	readChan, daemonChan := make(chan bool), make(chan bool)

	go func() {
		for {
			time.Sleep(pingInterval * time.Second)

			select {
			case <-daemonChan:
				return
			default:
				now := time.Now().UnixNano()

				// Garbage collect all closed and/or inactive streams
				cs.streams.Iterate(func(idx uint32, p unsafe.Pointer) bool {
					s := (*Stream)(p)
					if s.closed.Load().(bool) {
						// return false to delete
						return false
					}

					// TODO
					if to := s.timeout; to > 0 && (now-s.lastActive)/1e9 <= to {
						return true
					}

					s.notifyRead(notifyCancel)
					s.notifyWrite(notifyCancel)
					return false
				})

				// Send ping
				if _, err := cs.conn.Write(makeFrame(0, cmdPing, nil)); err != nil {
					cs.broadcast(err)
					return
				}
			}
		}
	}()

	for {
		go func() {
			buf := make([]byte, 7)

			// Normally we have pings so this deadline shall never be met
			// cs.conn.SetReadDeadline(time.Now().Add(time.Duration(cs.timeout) * time.Second))
			_, err := io.ReadAtLeast(cs.conn, buf, 7)

			if err != nil {
				cs.broadcast(err)
				return
			}

			if buf[0] != Version {
				cs.broadcast(ErrInvalidVerHdr)
				return
			}

			streamIdx := binary.BigEndian.Uint32(buf[1:])
			streamLen := int(binary.BigEndian.Uint16(buf[5:]))

			if buf[5] == cmdByte && buf[6] != 0 {
				switch buf[6] {
				case cmdHello:
					// The stream will be added into connState in this callback
					cs.newStreamCallback(&readState{idx: streamIdx})

					buf[5], buf[6] = cmdByte, cmdAck
					// We acknowledge the hello
					if _, err = cs.conn.Write(buf); err != nil {
						cs.broadcast(err)
						return
					}

					fallthrough
				case cmdPing:
					readChan <- true
					return
				default:
					if p, ok := cs.streams.Load(streamIdx); ok {
						s, cmd := (*Stream)(p), buf[6]
						select {
						case s.writeStateResp <- cmd:
						default:
						}

						select {
						case s.readResp <- &readState{cmd: cmd}:
						default:
						}
					}
				}

				readChan <- true
				return
			}

			payload := make([]byte, streamLen)
			_, err = io.ReadAtLeast(cs.conn, payload, streamLen)
			// Maybe we will encounter an error, but we pass it to streams
			// Next loop when we read the header, we will have the error again, that time we will broadcast
			rs := &readState{
				n:   streamLen,
				err: err,
				buf: payload,
				idx: streamIdx,
			}

			if s, ok := cs.streams.Load(streamIdx); ok {
				(*Stream)(s).readResp <- rs
			} else {
				buf[5], buf[6] = cmdByte, cmdClose
				if _, err = cs.conn.Write(buf); err != nil {
					cs.broadcast(err)
					return
				}
			}
			readChan <- true
		}()

		select {
		case <-cs.exitRead:
			daemonChan <- true
			return
		default:
		}

		select {
		case <-readChan:
		case <-cs.exitRead:
			daemonChan <- true
			return
		}
	}
}

// Even all streams are closed, the conn will still not be removed from the master.
// It gets removed only if it encountered an error, and stop() was called, or any one of its streams called CloseMaster()
func (cs *connState) stop() {
	cs.Lock()
	if cs.stopped {
		cs.Unlock()
		return
	}

	cs.exitRead <- true
	cs.streams.Iterate(func(idx uint32, p unsafe.Pointer) bool {
		s := (*Stream)(p)
		s.closeNoInfo()
		return true
	})

	cs.conn.Close()
	cs.master.Delete(cs.idx)

	cs.stopped = true
	cs.Unlock()
}

// Conn can prefetch one byte from net.Conn before Read()
type Conn struct {
	data  uintptr
	len   int
	cap   int
	first byte
	err   error

	net.Conn
}

func (c *Conn) FirstByte() (b byte, err error) {
	if c.len == 1 {
		return c.first, c.err
	}

	var n int

	c.data = uintptr(unsafe.Pointer(c)) + strconv.IntSize/8*3
	c.len = 1
	c.cap = 1

	n, err = c.Conn.Read(*(*[]byte)(unsafe.Pointer(c)))
	c.err = err

	if n == 1 {
		b = c.first
	}

	return
}

func (c *Conn) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}

	if c.len == 1 {
		p[0] = c.first
		xp := p[1:]

		n, err := c.Conn.Read(xp)
		c.len = 0

		return n + 1, err
	}

	return c.Conn.Read(p)
}
