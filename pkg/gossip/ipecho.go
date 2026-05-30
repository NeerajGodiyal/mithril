package gossip

import (
	"fmt"
	"net"
	"time"
)

const (
	ipEchoHeaderLength   = 4
	ipEchoResponseLength = ipEchoHeaderLength + 23
)

type EchoResponse struct {
	Address      net.IP
	ShredVersion uint16
}

func QueryEntrypoint(entrypoint *net.UDPAddr, timeout time.Duration) (EchoResponse, error) {
	if entrypoint == nil {
		return EchoResponse{}, fmt.Errorf("nil gossip entrypoint")
	}
	tcpAddr := &net.TCPAddr{IP: entrypoint.IP, Port: entrypoint.Port, Zone: entrypoint.Zone}
	conn, err := net.DialTimeout("tcp", tcpAddr.String(), timeout)
	if err != nil {
		return EchoResponse{}, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return EchoResponse{}, err
	}

	req := make([]byte, 0, ipEchoHeaderLength+16+1)
	req = append(req, 0, 0, 0, 0)
	req = append(req, make([]byte, 16)...)
	req = append(req, '\n')
	if _, err := conn.Write(req); err != nil {
		return EchoResponse{}, err
	}

	resp := make([]byte, ipEchoResponseLength)
	n, err := conn.Read(resp)
	if err != nil {
		return EchoResponse{}, err
	}
	if n < ipEchoHeaderLength {
		return EchoResponse{}, fmt.Errorf("short IP echo response: %d bytes", n)
	}
	if resp[0] != 0 || resp[1] != 0 || resp[2] != 0 || resp[3] != 0 {
		return EchoResponse{}, fmt.Errorf("invalid IP echo response header %v", resp[:ipEchoHeaderLength])
	}

	d := newDecoder(resp[ipEchoHeaderLength:n])
	ip, err := decodeIP(d)
	if err != nil {
		return EchoResponse{}, err
	}
	hasShredVersion, err := d.bool()
	if err != nil {
		return EchoResponse{}, err
	}
	if !hasShredVersion {
		return EchoResponse{}, fmt.Errorf("entrypoint did not return a shred version")
	}
	shredVersion, err := d.u16()
	if err != nil {
		return EchoResponse{}, err
	}
	return EchoResponse{Address: normalizedIP(ip), ShredVersion: shredVersion}, nil
}
