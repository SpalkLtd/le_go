package le_go

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestLogger creates a Logger with a fakeConnection, suitable for tests
// that don't need real network connectivity.
func newTestLogger(token string, concurrentWrites int) *Logger {
	l := newEmptyLogger("", token, 0)
	l.conn = &fakeConnection{}
	l.errOutput = io.Discard
	if concurrentWrites > 0 {
		l.concurrentWrites = make(chan struct{}, concurrentWrites)
		for i := 0; i < concurrentWrites; i++ {
			l.concurrentWrites <- struct{}{}
		}
	}
	return &l
}

// startTLSServer starts a local TLS server for testing Connect/openConnection.
// Returns the server address and a client tls.Config that trusts the server's cert.
func startTLSServer(t *testing.T) (addr string, clientConfig *tls.Config) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Accept connections in the background. Track them so we can close them
	// on cleanup — but keep them open during the test so the TLS handshake
	// completes on the client side.
	var (
		connMu sync.Mutex
		conns  []net.Conn
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connMu.Lock()
			conns = append(conns, conn)
			connMu.Unlock()
			// Complete the TLS handshake so the client's tls.Dial returns.
			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.Handshake()
			}
		}
	}()

	t.Cleanup(func() {
		listener.Close()
		connMu.Lock()
		for _, c := range conns {
			c.Close()
		}
		connMu.Unlock()
	})

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)

	return listener.Addr().String(), &tls.Config{RootCAs: pool}
}

func TestConnectOpensConnection(t *testing.T) {
	addr, clientConfig := startTLSServer(t)

	// Connect uses a default tls.Config that won't trust our self-signed
	// cert, so we set up the logger manually and call openConnection — the
	// same code path Connect takes.
	le := newEmptyLogger(addr, "", 0)
	le.tlsConfig = clientConfig

	if err := le.openConnection(); err != nil {
		t.Fatal(err)
	}
	defer le.Close()

	if le.conn == nil {
		t.Fatal("expected conn to be non-nil after openConnection")
	}
}

func TestConnectSetsToken(t *testing.T) {
	le := newTestLogger("myToken", 0)

	if le.token != "myToken" {
		t.Fatalf("expected token 'myToken', got '%s'", le.token)
	}
}

func TestCloseClosesConnection(t *testing.T) {
	le := newTestLogger("", 0)

	le.Close()

	if le.conn != nil {
		t.Fatal("expected conn to be nil after Close")
	}
}

func TestOpenConnectionOpensConnection(t *testing.T) {
	addr, clientConfig := startTLSServer(t)

	le := newEmptyLogger(addr, "", 0)
	le.tlsConfig = clientConfig

	if err := le.openConnection(); err != nil {
		t.Fatal(err)
	}
	defer le.Close()

	if le.conn == nil {
		t.Fatal("expected conn to be non-nil after openConnection")
	}
}

func TestEnsureOpenConnectionDoesNothingOnOpenConnection(t *testing.T) {
	le := newTestLogger("", 0)
	existingConn := le.conn

	le.ensureOpenConnection()

	if le.conn != existingConn {
		t.Fatal("expected ensureOpenConnection to keep existing conn")
	}
}

func TestEnsureOpenConnectionCreatesNewConnection(t *testing.T) {
	addr, clientConfig := startTLSServer(t)

	le := newEmptyLogger(addr, "", 0)
	le.tlsConfig = clientConfig
	// conn starts nil
	if le.conn != nil {
		t.Fatal("expected conn to start nil")
	}

	if err := le.ensureOpenConnection(); err != nil {
		t.Fatal(err)
	}
	defer le.Close()

	if le.conn == nil {
		t.Fatal("expected conn to be non-nil after ensureOpenConnection")
	}
}

func TestFlagsReturnsFlag(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.flag = 2

	if le.Flags() != 2 {
		t.Fail()
	}
}

func TestSetFlagsSetsFlag(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.flag = 2

	le.SetFlags(1)

	if le.flag != 1 {
		t.Fail()
	}
}

func TestPrefixReturnsPrefix(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.prefix = "myPrefix"

	if le.Prefix() != "myPrefix" {
		t.Fail()
	}
}

func TestSetPrefixSetsPrefix(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.prefix = "myPrefix"

	le.SetPrefix("myNewPrefix")

	if le.prefix != "myNewPrefix" {
		t.Fail()
	}
}

func TestLoggerImplementsWriterInterface(t *testing.T) {
	le := newTestLogger("myToken", 0)
	defer le.Close()

	// the test will fail to compile if Logger doesn't implement io.Writer
	func(w io.Writer) {}(le)
}

func TestReplaceNewline(t *testing.T) {
	le := newTestLogger("myToken", 0)
	defer le.Close()

	le._testWaitForWrite = &sync.WaitGroup{}
	le._testWaitForWrite.Add(1)

	le.Println("1\n2\n3")

	le._testWaitForWrite.Wait()

	if strings.Count(string(le.buf), "\u2028") != 2 {
		t.Fail()
	}
}

func TestAddNewline(t *testing.T) {
	le := newTestLogger("myToken", 0)
	defer le.Close()

	le._testWaitForWrite = &sync.WaitGroup{}
	le._testWaitForWrite.Add(1)

	le.Print("123")

	le._testWaitForWrite.Wait()

	if !strings.HasSuffix(string(le.buf), "\n") {
		t.Fail()
	}

	le._testWaitForWrite.Add(1)

	le.Printf("%s", "123")

	le._testWaitForWrite.Wait()

	if !strings.HasSuffix(string(le.buf), "\n") {
		t.Fail()
	}
}

func TestCanSendMoreThan64k(t *testing.T) {
	le := newTestLogger("myToken", 0)
	defer le.Close()

	le._testWaitForWrite = &sync.WaitGroup{}
	le._testWaitForWrite.Add(3) // 3 because we need to write 3 times since it exceeds the limit (64k)

	longBytes := make([]byte, 140000)
	for i := 0; i < 140000; i++ {
		longBytes[i] = 'a'
	}
	longString := string(longBytes)
	// Fake the connection so we can hear about it
	fakeConn := fakeConnection{}
	le.conn = &fakeConn
	le.Print(longString)

	le._testWaitForWrite.Wait()

	if fakeConn.WriteCalls < 2 {
		t.Fail()
	}
}

func TestTimeoutWrites(t *testing.T) {
	le := newTestLogger("myToken", 0)
	defer le.Close()

	timedoutCount := 0
	le._testTimedoutWrite = func() {
		timedoutCount++
	}

	le._testWaitForWrite = &sync.WaitGroup{}
	le._testWaitForWrite.Add(1)

	b := make([]byte, 1000)
	for i := 0; i < 1000; i++ {
		b[i] = 'a'
	}
	s := string(b)
	// Fake the connection so we can hear about it
	fakeConn := fakeConnection{
		writeDuration: 12 * time.Second,
	}
	le.conn = &fakeConn
	go le.Print(s)
	go le.Print(s)

	le._testWaitForWrite.Wait()

	if fakeConn.SetWriteTimeoutCalls < 1 {
		t.Fail()
	}
	if timedoutCount < 1 {
		t.Fail()
	}
}

func TestLimitedConcurrentWrites(t *testing.T) {
	le := newTestLogger("myToken", 3)
	defer le.Close()

	timedoutCount := 0
	le._testTimedoutWrite = func() {
		timedoutCount++
	}

	le._testWaitForWrite = &sync.WaitGroup{}
	le._testWaitForWrite.Add(1)

	b := make([]byte, 1000)
	for i := 0; i < 1000; i++ {
		b[i] = 'a'
	}
	s := string(b)
	// Fake the connection so we can hear about it
	fakeConn := fakeConnection{
		writeDuration: 1 * time.Second,
	}
	le.conn = &fakeConn
	le.writeTimeout = 500 * time.Millisecond
	for i := 0; i < 100; i++ {
		go le.Print(s)
	}

	le._testWaitForWrite.Wait()

	if fakeConn.SetWriteTimeoutCalls < 1 {
		t.Fatalf("SetWriteTimeoutCalls should be > 1, got %d", fakeConn.SetWriteTimeoutCalls)
	}
	if fakeConn.SetWriteTimeoutCalls > 3 {
		t.Fatalf("SetWriteTimeoutCalls should be 3, got %d", fakeConn.SetWriteTimeoutCalls)
	}
	if timedoutCount < 1 {
		t.Fatalf("timedoutCount should be > 1, got %d", timedoutCount)
	}
	//Note only 3 timeouts when we have 100 writes, because we only have 3 concurrent writes
	if timedoutCount > 3 {
		t.Fatalf("timedoutCount should be 3, got %d", timedoutCount)
	}
}

type fakeConnection struct {
	WriteCalls           int
	SetWriteTimeoutCalls int
	writeDuration        time.Duration
}

func (f *fakeConnection) Write(b []byte) (int, error) {
	<-time.After(f.writeDuration)
	f.WriteCalls++
	return len(b), nil
}

func (f *fakeConnection) SetWriteDeadline(t time.Time) error {
	f.SetWriteTimeoutCalls++
	return nil
}

func (*fakeConnection) Read(b []byte) (int, error) {
	return len(b), &fakeError{}
}

func (*fakeConnection) SetReadDeadline(time.Time) error { return nil }

func (*fakeConnection) Close() error                  { return nil }
func (*fakeConnection) LocalAddr() net.Addr           { return &fakeAddr{} }
func (*fakeConnection) RemoteAddr() net.Addr          { return &fakeAddr{} }
func (*fakeConnection) SetDeadline(t time.Time) error { return nil }

type fakeError struct{}

func (*fakeError) Error() string {
	return "fake network error"
}

func (*fakeError) Timeout() bool {
	return true
}

func (*fakeError) Temporary() bool {
	return true
}

type fakeAddr struct{}

func (f *fakeAddr) Network() string { return "" }
func (f *fakeAddr) String() string  { return "" }

func ExampleLogger() {
	le, err := Connect("data.logentries.com:443", "XXXX-XXXX-XXXX-XXXX", 0, os.Stderr, 0) // replace with token
	if err != nil {
		panic(err)
	}

	defer le.Close()

	le.Println("another test message")
}

func ExampleLogger_write() {
	le, err := Connect("data.logentries.com:443", "XXXX-XXXX-XXXX-XXXX", 0, os.Stderr, 0) // replace with token
	if err != nil {
		panic(err)
	}

	defer le.Close()

	fmt.Fprintln(le, "another test message")
}
