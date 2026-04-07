package le_go

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLogentriesServer is a local TLS server that mimics Logentries.
// It accepts connections, reads newline-delimited log lines, and records
// them for assertion. It also tracks how many connections were accepted.
type fakeLogentriesServer struct {
	listener  net.Listener
	tlsConfig *tls.Config
	addr      string

	mu    sync.Mutex
	lines []string // all received log lines
	conns []net.Conn

	connCount atomic.Int64

	// configurable behaviors — set these BEFORE calling newFakeLogentriesServer
	// or use the atomic setters below.
	readDelayAtomic  atomic.Int64 // nanoseconds
	rejectConns      atomic.Bool
	closeMidRead     atomic.Bool

	wg     sync.WaitGroup // tracks all server goroutines
	closed atomic.Bool
}

// SetReadDelay sets the delay before reading each line (thread-safe).
func (s *fakeLogentriesServer) SetReadDelay(d time.Duration) {
	s.readDelayAtomic.Store(int64(d))
}

func (s *fakeLogentriesServer) getReadDelay() time.Duration {
	return time.Duration(s.readDelayAtomic.Load())
}

func newFakeLogentriesServer(t *testing.T) *fakeLogentriesServer {
	t.Helper()

	tlsConfig := generateTLSConfig(t)

	listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatalf("failed to start fake server: %v", err)
	}

	s := &fakeLogentriesServer{
		listener:  listener,
		tlsConfig: tlsConfig,
		addr:      listener.Addr().String(),
	}

	s.wg.Add(1)
	go s.acceptLoop()

	return s
}

func (s *fakeLogentriesServer) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			continue
		}

		s.connCount.Add(1)

		if s.rejectConns.Load() {
			conn.Close()
			continue
		}

		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *fakeLogentriesServer) handleConn(conn net.Conn) {
	defer s.wg.Done()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if s.closed.Load() {
			return
		}

		if d := s.getReadDelay(); d > 0 {
			time.Sleep(d)
		}

		line := scanner.Text()
		s.mu.Lock()
		s.lines = append(s.lines, line)
		s.mu.Unlock()

		if s.closeMidRead.Load() {
			conn.Close()
			return
		}
	}
}

// Lines returns a copy of all received log lines.
func (s *fakeLogentriesServer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, len(s.lines))
	copy(result, s.lines)
	return result
}

// ConnectionCount returns how many connections were accepted.
func (s *fakeLogentriesServer) ConnectionCount() int {
	return int(s.connCount.Load())
}

// Close shuts down the server and waits for all goroutines to finish.
func (s *fakeLogentriesServer) Close() {
	s.closed.Store(true)
	s.listener.Close()

	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()

	s.wg.Wait()
}

// connectToFakeServer creates a Logger connected to the fake server.
func connectToFakeServer(t *testing.T, s *fakeLogentriesServer, concurrentWrites int, errOutput interface{ Write([]byte) (int, error) }) *Logger {
	t.Helper()

	logger := newEmptyLogger(s.addr, "test-token", 0)
	if concurrentWrites > 0 {
		logger.concurrentWrites = make(chan struct{}, concurrentWrites)
		for i := 0; i < concurrentWrites; i++ {
			logger.concurrentWrites <- struct{}{}
		}
	}
	if errOutput != nil {
		logger.errOutput = errOutput
	} else {
		logger.errOutput = &discardWriter{}
	}

	// Configure TLS for self-signed cert (used both for initial and reconnections)
	logger.tlsConfig = &tls.Config{InsecureSkipVerify: true}

	conn, err := tls.Dial("tcp", s.addr, logger.tlsConfig)
	if err != nil {
		t.Fatalf("failed to connect to fake server: %v", err)
	}
	logger.conn = conn
	logger.lastRefreshAt = time.Now()

	return logger
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// generateTLSConfig creates a self-signed TLS config for testing.
func generateTLSConfig(t *testing.T) *tls.Config {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Test"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("failed to load key pair: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
}
