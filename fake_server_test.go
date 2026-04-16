package le_go

import (
	"bufio"
	"crypto/ecdsa"
	"strings"
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

type fakeLogentriesServer struct {
	listener  net.Listener
	tlsConfig *tls.Config
	addr      string

	mu    sync.Mutex
	lines []string
	conns []net.Conn

	connCount atomic.Int64

	readDelayAtomic atomic.Int64
	rejectConns     atomic.Bool
	closeMidRead    atomic.Bool

	wg     sync.WaitGroup
	closed atomic.Bool
}

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

func (s *fakeLogentriesServer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, len(s.lines))
	copy(result, s.lines)
	return result
}

func (s *fakeLogentriesServer) ConnectionCount() int {
	return int(s.connCount.Load())
}

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

func connectToFakeServer(t *testing.T, s *fakeLogentriesServer, concurrentWrites int, errOutput interface{ Write([]byte) (int, error) }, opts ...Option) *Logger {
	t.Helper()

	capacity := concurrentWrites
	if capacity <= 0 {
		capacity = 4096
	}

	l := &Logger{
		host:            s.addr,
		token:           "test-token",
		calldepthOffset: 0,
		writeTimeout:    defaultWriteTimeout,
		dialTimeout:     defaultDialTimeout,
		ch:              make(chan entry, capacity),
		done:            make(chan struct{}),
		exited:          make(chan struct{}),
		lastRefreshAt:   time.Now(),
		tlsConfig:       &tls.Config{InsecureSkipVerify: true},
		_testDropped:    func(DropReason) {},
	}

	if errOutput != nil {
		l.errOutput = errOutput
	} else {
		l.errOutput = &discardWriter{}
	}

	for _, opt := range opts {
		opt(l)
	}

	conn, err := tls.Dial("tcp", s.addr, l.tlsConfig)
	if err != nil {
		t.Fatalf("failed to connect to fake server: %v", err)
	}
	l.conn = conn
	l.lastRefreshAt = time.Now()

	go l.run()
	return l
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

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

func waitForLines(t *testing.T, s *fakeLogentriesServer, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(s.Lines()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d lines, got %d", n, len(s.Lines()))
}

func waitForConnections(t *testing.T, s *fakeLogentriesServer, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.ConnectionCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d connections, got %d", n, s.ConnectionCount())
}

func stripTokenAndHeader(line, token string) string {
	prefix := token + " "
	if !strings.HasPrefix(line, prefix) {
		return line
	}
	return line[len(prefix):]
}
