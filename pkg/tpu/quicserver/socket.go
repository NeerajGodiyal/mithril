package quicserver

import (
	"fmt"
	"net"
)

type QuicSocket struct {
	conn *net.UDPConn
}

func QuicSocketFromUDP(conn *net.UDPConn) QuicSocket {
	return QuicSocket{conn: conn}
}

func (s QuicSocket) UDPConn() *net.UDPConn {
	return s.conn
}

func (s QuicSocket) LocalAddr() (net.Addr, error) {
	if s.conn == nil {
		return nil, fmt.Errorf("quic socket is nil")
	}
	return s.conn.LocalAddr(), nil
}

func (s QuicSocket) String() string {
	if s.conn == nil {
		return "<nil>"
	}
	return s.conn.LocalAddr().String()
}
