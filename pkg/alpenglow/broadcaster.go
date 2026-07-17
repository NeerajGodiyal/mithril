package alpenglow

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	defaultVotorBroadcastQueue = 1024
	defaultVotorSendWorkers    = 32
	defaultVotorPeerJobQueue   = 16384
)

type VotorPeerSource func(slot uint64) []*net.UDPAddr

type VotorBroadcasterConfig struct {
	Identity     ed25519.PrivateKey
	ShredVersion uint16
	Peers        VotorPeerSource
	QueueSize    int
	Workers      int
}

type VotorBroadcasterStats struct {
	MessagesQueued  uint64
	MessagesDropped uint64
	PeerSends       uint64
	PeerSendErrors  uint64
	Connections     int
}

type votorPeerJob struct {
	addr    *net.UDPAddr
	payload []byte
}

type VotorBroadcaster struct {
	shredVersion uint16
	peers        VotorPeerSource
	tlsConfig    *tls.Config
	quicConfig   *quic.Config
	queue        chan Message
	jobs         chan votorPeerJob
	done         chan struct{}
	closeOnce    sync.Once
	wg           sync.WaitGroup
	closed       atomic.Bool
	connMu       sync.Mutex
	conns        map[string]*quic.Conn
	queued       atomic.Uint64
	dropped      atomic.Uint64
	sends        atomic.Uint64
	sendErrors   atomic.Uint64
}

func NewVotorBroadcaster(cfg VotorBroadcasterConfig) (*VotorBroadcaster, error) {
	if len(cfg.Identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("Votor broadcaster: validator identity is required")
	}
	if cfg.Peers == nil {
		return nil, fmt.Errorf("Votor broadcaster: peer source is required")
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultVotorBroadcastQueue
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultVotorSendWorkers
	}
	certificate, err := newVotorQUICCertificate(cfg.Identity)
	if err != nil {
		return nil, fmt.Errorf("Votor broadcaster certificate: %w", err)
	}
	b := &VotorBroadcaster{
		shredVersion: cfg.ShredVersion,
		peers:        cfg.Peers,
		tlsConfig: &tls.Config{
			Certificates:       []tls.Certificate{certificate},
			NextProtos:         []string{VotorQUICALPN},
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // Solana A2A uses identity certs, not WebPKI names.
		},
		quicConfig: &quic.Config{
			HandshakeIdleTimeout: 2 * time.Second,
			MaxIdleTimeout:       30 * time.Second,
			KeepAlivePeriod:      5 * time.Second,
		},
		queue: make(chan Message, cfg.QueueSize),
		jobs:  make(chan votorPeerJob, defaultVotorPeerJobQueue),
		done:  make(chan struct{}),
		conns: make(map[string]*quic.Conn),
	}
	b.wg.Add(1 + cfg.Workers)
	go b.broadcastLoop()
	for range cfg.Workers {
		go b.sendLoop()
	}
	return b, nil
}

// Enqueue broadcasts a vote or certificate to every staked validator contact
// returned for the message's epoch. A full local queue is surfaced to the
// caller; the voting engine treats dropping a newly signed vote as fatal.
func (b *VotorBroadcaster) Enqueue(message Message) error {
	if b == nil {
		return fmt.Errorf("Votor broadcaster is nil")
	}
	if b.closed.Load() {
		return fmt.Errorf("Votor broadcaster is closed")
	}
	message.ShredVersion = b.shredVersion
	if err := message.ValidateBasic(); err != nil {
		return err
	}
	select {
	case <-b.done:
		return fmt.Errorf("Votor broadcaster is closed")
	case b.queue <- message:
		b.queued.Add(1)
		return nil
	default:
		b.dropped.Add(1)
		return fmt.Errorf("Votor broadcaster queue is full")
	}
}

func (b *VotorBroadcaster) broadcastLoop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.done:
			return
		case message := <-b.queue:
			payload, err := EncodeMessage(message)
			if err != nil {
				b.sendErrors.Add(1)
				continue
			}
			for _, peer := range b.peers(message.Slot()) {
				if peer == nil || peer.Port == 0 || peer.IP == nil || peer.IP.IsUnspecified() {
					continue
				}
				job := votorPeerJob{addr: cloneUDPAddr(peer), payload: payload}
				select {
				case b.jobs <- job:
				case <-b.done:
					return
				default:
					b.dropped.Add(1)
				}
			}
		}
	}
}

func (b *VotorBroadcaster) sendLoop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.done:
			return
		case job := <-b.jobs:
			if err := b.send(job.addr, job.payload); err != nil {
				b.sendErrors.Add(1)
			} else {
				b.sends.Add(1)
			}
		}
	}
}

func (b *VotorBroadcaster) send(addr *net.UDPAddr, payload []byte) error {
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := b.connection(addr)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		stream, err := conn.OpenUniStreamSync(ctx)
		if err == nil {
			_, err = stream.Write(payload)
			if closeErr := stream.Close(); err == nil {
				err = closeErr
			}
		}
		cancel()
		if err == nil {
			return nil
		}
		b.dropConnection(addr.String(), conn)
	}
	return fmt.Errorf("send Votor message to %s failed after reconnect", addr)
}

func (b *VotorBroadcaster) connection(addr *net.UDPAddr) (*quic.Conn, error) {
	if b.closed.Load() {
		return nil, fmt.Errorf("Votor broadcaster is closed")
	}
	key := addr.String()
	b.connMu.Lock()
	if conn := b.conns[key]; conn != nil && conn.Context().Err() == nil {
		b.connMu.Unlock()
		return conn, nil
	}
	b.connMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, key, b.tlsConfig.Clone(), b.quicConfig.Clone())
	if err != nil {
		return nil, err
	}
	if b.closed.Load() {
		_ = conn.CloseWithError(0, "Votor broadcaster closed")
		return nil, fmt.Errorf("Votor broadcaster is closed")
	}
	b.connMu.Lock()
	if b.closed.Load() {
		b.connMu.Unlock()
		_ = conn.CloseWithError(0, "Votor broadcaster closed")
		return nil, fmt.Errorf("Votor broadcaster is closed")
	}
	if existing := b.conns[key]; existing != nil && existing.Context().Err() == nil {
		b.connMu.Unlock()
		_ = conn.CloseWithError(0, "duplicate Votor connection")
		return existing, nil
	}
	b.conns[key] = conn
	b.connMu.Unlock()
	return conn, nil
}

func (b *VotorBroadcaster) dropConnection(key string, conn *quic.Conn) {
	b.connMu.Lock()
	if b.conns[key] == conn {
		delete(b.conns, key)
	}
	b.connMu.Unlock()
	_ = conn.CloseWithError(0, "Votor send failed")
}

func (b *VotorBroadcaster) Stats() VotorBroadcasterStats {
	if b == nil {
		return VotorBroadcasterStats{}
	}
	b.connMu.Lock()
	connections := len(b.conns)
	b.connMu.Unlock()
	return VotorBroadcasterStats{
		MessagesQueued:  b.queued.Load(),
		MessagesDropped: b.dropped.Load(),
		PeerSends:       b.sends.Load(),
		PeerSendErrors:  b.sendErrors.Load(),
		Connections:     connections,
	}
}

func (b *VotorBroadcaster) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.done)
		b.connMu.Lock()
		for key, conn := range b.conns {
			_ = conn.CloseWithError(0, "Votor broadcaster closed")
			delete(b.conns, key)
		}
		b.connMu.Unlock()
		b.wg.Wait()
	})
	return nil
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	ip := append(net.IP(nil), addr.IP...)
	return &net.UDPAddr{IP: ip, Port: addr.Port, Zone: addr.Zone}
}
