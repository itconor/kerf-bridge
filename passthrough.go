package main

import (
	"net"
	"sync"
)

// rawClient is one TCP connection on the raw port. The first one, when the
// laser is free, gets control and talks straight to GRBL, exactly as with
// ser2net — LightBurn's "GRBL over TCP/IP" works unchanged. Anyone else
// watches: they see everything the laser says, and the only bytes of theirs
// that reach it are feed hold, reset and status, so any of them can still
// stop the laser.
type rawClient struct {
	conn    net.Conn
	out     chan []byte
	control bool
	once    sync.Once
}

func (c *rawClient) send(b []byte) {
	cp := append([]byte(nil), b...)
	select {
	case c.out <- cp:
	default:
		// A client too slow to keep up is dropped rather than allowed to
		// stall the serial reader (and with it, every job).
		c.close()
	}
}

func (c *rawClient) close() {
	c.once.Do(func() { c.conn.Close() })
}

func ServeRaw(m *Machine, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go m.serveRawClient(conn)
	}
}

func (m *Machine) serveRawClient(conn net.Conn) {
	c := &rawClient{conn: conn, out: make(chan []byte, 256)}

	m.mu.Lock()
	switch {
	case m.port == nil:
		m.mu.Unlock()
		conn.Write([]byte("[MSG:Kerf Bridge: no laser connected]\r\n"))
		conn.Close()
		return
	case m.controller == ControllerNone && len(m.flight) == 0:
		m.controller = ControllerPassthrough
		c.control = true
	default:
		conn.Write([]byte("[MSG:Kerf Bridge: watching only, " + m.busyReason() + "]\r\n"))
	}
	m.sinks[c] = true
	m.mu.Unlock()
	m.logf("raw client %s connected (control: %v)", conn.RemoteAddr(), c.control)

	go func() {
		for b := range c.out {
			if _, err := conn.Write(b); err != nil {
				c.close()
				return
			}
		}
	}()

	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			m.fromRawClient(c, buf[:n])
		}
		if err != nil {
			break
		}
	}

	m.mu.Lock()
	delete(m.sinks, c)
	if c.control && m.controller == ControllerPassthrough {
		m.controller = ControllerNone
	}
	m.mu.Unlock()
	close(c.out)
	c.close()
	m.logf("raw client %s disconnected", conn.RemoteAddr())
}

func (m *Machine) fromRawClient(c *rawClient, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.control {
		m.writeLocked(b)
		return
	}
	// Watchers: safety bytes only.
	for _, x := range b {
		if x == rtHold || x == rtReset || x == rtStatus {
			m.writeLocked([]byte{x})
		}
	}
}
