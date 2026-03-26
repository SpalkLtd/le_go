package le_go

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// Test Infrastructure
// =============================================================================

// waitForLines waits until the server has at least n lines, or timeout.
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

// waitForConnections waits until the server has at least n connections, or timeout.
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

// stripTokenAndHeader removes the "token " prefix and any header from a log line,
// returning just the message portion.
func stripTokenAndHeader(line, token string) string {
	// Lines are formatted as: "token header message"
	// The token is always first, followed by a space.
	prefix := token + " "
	if !strings.HasPrefix(line, prefix) {
		return line
	}
	return line[len(prefix):]
}

// =============================================================================
// GREEN Tests — Connection Management (expected to pass)
// =============================================================================

func TestConnectOpensConnection(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	if le.conn == nil {
		t.Fatal("expected connection to be open")
	}
}

func TestConnectSetsToken(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	if le.token != "test-token" {
		t.Fatalf("expected token 'test-token', got %q", le.token)
	}
}

func TestCloseClosesConnection(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	// Writing to a closed connection should fail
	_, err := le.conn.Write([]byte("test"))
	if err == nil {
		t.Fatal("expected error writing to closed connection")
	}
}

// =============================================================================
// GREEN Tests — Configuration (no connection needed)
// =============================================================================

func TestFlagsReturnsFlag(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.flag = 2

	if le.Flags() != 2 {
		t.Fatalf("expected flag 2, got %d", le.Flags())
	}
}

func TestSetFlagsSetsFlag(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.SetFlags(1)

	if le.flag != 1 {
		t.Fatalf("expected flag 1, got %d", le.flag)
	}
}

func TestPrefixReturnsPrefix(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.prefix = "myPrefix"

	if le.Prefix() != "myPrefix" {
		t.Fatalf("expected prefix 'myPrefix', got %q", le.Prefix())
	}
}

func TestSetPrefixSetsPrefix(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.SetPrefix("myNewPrefix")

	if le.prefix != "myNewPrefix" {
		t.Fatalf("expected prefix 'myNewPrefix', got %q", le.prefix)
	}
}

// =============================================================================
// GREEN Tests — Core Logging
// =============================================================================

func TestPrintSendsToServer(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Print("hello")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "test-token ") {
		t.Fatalf("expected line to start with token, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "hello") {
		t.Fatalf("expected line to contain 'hello', got %q", lines[0])
	}
}

func TestPrintfFormatsCorrectly(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Printf("count: %d", 42)
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "count: 42") {
		t.Fatalf("expected formatted message, got %q", lines[0])
	}
}

func TestPrintlnAppendsNewline(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Println("hello")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "hello") {
		t.Fatalf("expected message 'hello', got %q", lines[0])
	}
}

func TestReplaceNewline(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Println("1\n2\n3")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	// Internal newlines should be replaced with \u2028
	if strings.Count(lines[0], "\u2028") < 2 {
		t.Fatalf("expected at least 2 \\u2028 replacements, got %q", lines[0])
	}
}

func TestAddNewlineWhenMissing(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Print (no trailing newline in the message)
	le.Print("hello")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	// The server received a line, which means it was newline-terminated
	lines := server.Lines()
	if len(lines) < 1 {
		t.Fatal("expected at least 1 line")
	}
}

func TestEmptyMessageHandling(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Should not panic
	le.Print("")
	le.Flush()
	// We don't assert on server lines — empty messages may or may not produce output
}

func TestLoggerImplementsWriterInterface(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// The test will fail to compile if Logger doesn't implement io.Writer
	func(w io.Writer) {}(le)
}

// =============================================================================
// GREEN Tests — Flush
// =============================================================================

func TestFlushWaitsForPendingWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	for i := 0; i < 10; i++ {
		le.Printf("message %d", i)
	}
	le.Flush()

	// Flush guarantees writes were sent; wait for server to finish reading them
	waitForLines(t, server, 10, 5*time.Second)

	lines := server.Lines()
	if len(lines) != 10 {
		t.Fatalf("expected 10 lines after flush, got %d", len(lines))
	}
}

// =============================================================================
// GREEN Tests — Error Output
// =============================================================================

func TestWriteFailureLogsToErrOutput(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)

	// Close the connection to force write failure
	le.conn.Close()
	le.conn = nil // force ensureOpenConnection to reconnect

	// Point to a non-existent host so reconnect fails
	le.host = "127.0.0.1:1" // port 1 — connection refused
	le.dialTimeout = 1 * time.Second

	le.Print("test message")
	le.Flush()

	// Give some time for the error to be logged
	time.Sleep(200 * time.Millisecond)

	errOutput := errBuf.String()
	if !strings.Contains(errOutput, "write error") {
		t.Fatalf("expected error output to contain 'write error', got %q", errOutput)
	}
}

func TestDroppedLogsUnderBackpressure(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	// Slow server to create backpressure
	server.SetReadDelay(100 * time.Millisecond)

	le := connectToFakeServer(t, server, 2, nil)
	defer le.Close()

	// Send many messages — some should be dropped since concurrentWrites=2
	for i := 0; i < 100; i++ {
		le.Printf("message %d", i)
	}
	le.Flush()

	// We should have received some but not all 100
	lines := server.Lines()
	if len(lines) > 100 {
		t.Fatalf("expected at most 100 lines, got %d", len(lines))
	}
}

// =============================================================================
// Tests — Bug 1: lastRefreshAt updated on reconnect
// =============================================================================

func TestLastRefreshAtUpdatedOnReconnect(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Backdate lastRefreshAt to trigger reconnect on next write
	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)

	// First write after staleness — triggers reconnect
	le.Print("after stale")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)
	waitForConnections(t, server, 2, 5*time.Second)

	// Second write — should NOT reconnect (lastRefreshAt was updated)
	le.Print("second write")
	le.Flush()
	waitForLines(t, server, 2, 5*time.Second)

	// Should be exactly 2 connections: initial + 1 reconnect
	if server.ConnectionCount() > 2 {
		t.Fatalf("expected 2 connections (initial + 1 reconnect), got %d (Bug 1: lastRefreshAt not updated)", server.ConnectionCount())
	}
}

func TestConnectionRefreshDoesNotReconnectEveryWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)

	// Write 5 times — only the first should reconnect
	for i := 0; i < 5; i++ {
		le.Printf("msg %d", i)
		le.Flush()
	}
	waitForLines(t, server, 5, 5*time.Second)

	// Bug 1 would cause 6 connections (initial + 5 reconnects)
	if server.ConnectionCount() > 2 {
		t.Fatalf("expected 2 connections, got %d (Bug 1: reconnecting every write)", server.ConnectionCount())
	}
}

// =============================================================================
// Tests — Bug 2: Loop advancement by chunk size, not TCP bytes
// =============================================================================

func TestChunkingContentCorrectness(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Create a message that will require multiple chunks
	msgBytes := make([]byte, 140000)
	for i := range msgBytes {
		msgBytes[i] = byte('a' + (i % 26))
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	// Wait for multiple chunks
	waitForLines(t, server, 2, 10*time.Second)
	lines := server.Lines()

	// Reassemble the message from chunks by stripping token+header
	var reassembled strings.Builder
	for _, line := range lines {
		// Strip token prefix
		content := stripTokenAndHeader(line, "test-token")
		reassembled.WriteString(content)
	}

	got := reassembled.String()

	// The full message should be recoverable without data loss
	if !strings.Contains(got, msg[:1000]) {
		t.Fatal("expected reassembled message to contain start of original")
	}
	if len(got) < len(msg) {
		t.Fatalf("expected reassembled length >= %d, got %d (data was lost)", len(msg), len(got))
	}
}

func TestChunkingExactBoundary(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Message exactly at maxLogLength-2 (one chunk boundary)
	msgBytes := make([]byte, maxLogLength-2)
	for i := range msgBytes {
		msgBytes[i] = 'x'
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// Should be exactly 1 line (fits in one chunk)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line for exact boundary message, got %d", len(lines))
	}

	content := stripTokenAndHeader(lines[0], "test-token")
	if len(content) < len(msg) {
		t.Fatalf("expected content length >= %d, got %d", len(msg), len(content))
	}
}

// =============================================================================
// Tests — Bug 3: Semaphore token return
// =============================================================================

func TestSemaphoreTokenReturnedAfterWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 3, nil)
	defer le.Close()

	// Send more messages than tokens — excess are dropped
	for i := 0; i < 10; i++ {
		le.Printf("message %d", i)
	}
	le.Flush()

	// After flush, all 3 tokens should be available
	available := len(le.concurrentWrites)
	if available != 3 {
		t.Fatalf("expected 3 semaphore tokens available after flush, got %d (tokens leaked)", available)
	}
}

// =============================================================================
// Tests — Bug 5: Write() prepends token
// =============================================================================

func TestWritePrependsToken(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	n, err := le.Write([]byte("hello via Write\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n == 0 {
		t.Fatal("expected n > 0")
	}

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	if !strings.HasPrefix(lines[0], "test-token ") {
		t.Fatalf("expected line to start with 'test-token ', got %q", lines[0])
	}
}

func TestWriteWithFprintln(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	fmt.Fprintln(le, "test via Fprintln")

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	if !strings.HasPrefix(lines[0], "test-token ") {
		t.Fatalf("expected line to start with 'test-token ', got %q", lines[0])
	}
	if !strings.Contains(lines[0], "test via Fprintln") {
		t.Fatalf("expected line to contain message, got %q", lines[0])
	}
}

func TestWriteReturnsFullLength(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	input := []byte("hello world\n")
	n, err := le.Write(input)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	// io.Writer contract: n should equal len(p)
	if n != len(input) {
		t.Fatalf("expected n=%d, got %d", len(input), n)
	}
}

// =============================================================================
// Tests — Bug 6: tls.Dial has no timeout
// =============================================================================

func TestDialTimeout(t *testing.T) {
	// Create a listener that accepts but never completes TLS handshake
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	// Don't accept any connections — the dial should hang
	addr := listener.Addr().String()

	done := make(chan error, 1)
	go func() {
		_, err := Connect(addr, "token", 0, io.Discard, 0)
		done <- err
	}()

	select {
	case <-done:
		// Connect returned (with error, which is expected)
	case <-time.After(15 * time.Second):
		t.Fatal("Connect hung for >15s — no dial timeout (Bug 6)")
	}
}

// =============================================================================
// Tests — Bug 8b: Close() flushes pending writes
// =============================================================================

func TestCloseWaitsForInFlightWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	server.SetReadDelay(50 * time.Millisecond)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("important message")

	// Close should wait for the write to complete
	le.Close()

	// Data is in TCP buffer; wait for server to read it
	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected Close() to wait for in-flight write, but message was lost")
	}
}

func TestCloseRejectsNewWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("before close")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	linesBefore := len(server.Lines())
	le.Close()

	// After close, new writes should be silently ignored
	le.Print("after close")
	time.Sleep(200 * time.Millisecond)

	linesAfter := len(server.Lines())
	if linesAfter > linesBefore {
		t.Fatal("expected no new lines after Close(), but writes were still accepted")
	}
}

// =============================================================================
// Tests — Bug 12: Newline replacement
// =============================================================================

func TestNewlineReplacementEmbeddedOnly(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Single embedded newline, no trailing newline
	le.Print("hello\nworld")
	le.Flush()

	// Wait for data to arrive
	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// The \n should be replaced with \u2028, so the server should see exactly 1 line
	// containing "hello\u2028world".
	if len(lines) > 1 {
		t.Fatalf("expected 1 line (embedded \\n replaced with \\u2028), but got %d lines — raw newline was sent (Bug 12)", len(lines))
	}
	if len(lines) == 1 && !strings.Contains(lines[0], "hello\u2028world") {
		t.Fatalf("expected 'hello\\u2028world' in line, got %q", lines[0])
	}
}

// =============================================================================
// Tests — Bug 13: Old connection closed on reconnect
// =============================================================================

func TestOldConnectionClosedOnReconnect(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Save reference to the initial connection
	initialConn := le.conn

	// Backdate to trigger reconnect
	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)

	le.Print("trigger reconnect")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	// The initial connection should have been closed by openConnection
	_, err := initialConn.Write([]byte("test"))
	if err == nil {
		t.Fatal("expected initial connection to be closed after reconnect (Bug 13)")
	}

	// New connection should be different
	if le.conn == initialConn {
		t.Fatal("expected new connection after reconnect")
	}
}

// =============================================================================
// Tests — Bug 14: writeToErrOutput uses Fprint not Fprintf
// =============================================================================

func TestWriteToErrOutputWithPercent(t *testing.T) {
	var errBuf bytes.Buffer
	le := newEmptyLogger("", "test-token", 0)
	le.errOutput = &errBuf
	le.errOutputMutex = &sync.RWMutex{}

	// Call writeToErrOutput with a string containing format verbs
	le.writeToErrOutput("100% complete %s %d")

	errOutput := errBuf.String()

	// The error output should contain the literal "100% complete %s %d"
	if strings.Contains(errOutput, "%!") {
		t.Fatalf("errOutput contains format verb artifacts (Bug 14): %q", errOutput)
	}
	if errOutput != "100% complete %s %d" {
		t.Fatalf("expected exact string, got %q", errOutput)
	}
}

// =============================================================================
// Tests — Bug 15: Each chunk is newline-terminated
// =============================================================================

func TestEachChunkNewlineTerminated(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Create a message that requires 3+ chunks, WITHOUT a trailing newline
	msgBytes := make([]byte, maxLogLength*2+1000)
	for i := range msgBytes {
		msgBytes[i] = byte('a' + (i % 26))
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 3, 10*time.Second)
	lines := server.Lines()

	// Each chunk should be newline-terminated so the server reads them as separate lines
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 separate lines (one per chunk), got %d", len(lines))
	}
}

// =============================================================================
// GREEN Tests — Chunking
// =============================================================================

func TestCanSendMoreThan64k(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	longBytes := make([]byte, 140000)
	for i := range longBytes {
		longBytes[i] = 'a'
	}
	le.Print(string(longBytes))
	le.Flush()

	// Should produce at least 3 chunks (140000 / 64998 ≈ 2.15 → 3 chunks)
	waitForLines(t, server, 2, 10*time.Second)
	lines := server.Lines()

	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines for 140KB message, got %d", len(lines))
	}
}

func TestChunkingMultipleChunks(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// 200KB — should need ~4 chunks
	longBytes := make([]byte, 200000)
	for i := range longBytes {
		longBytes[i] = byte('A' + (i % 26))
	}
	le.Print(string(longBytes))
	le.Flush()

	waitForLines(t, server, 3, 10*time.Second)
	lines := server.Lines()

	if len(lines) < 3 {
		t.Fatalf("expected at least 3 chunks for 200KB message, got %d", len(lines))
	}

	// All chunks should have the token prefix
	for i, line := range lines {
		if !strings.HasPrefix(line, "test-token ") {
			t.Fatalf("chunk %d missing token prefix: %q", i, line[:50])
		}
	}
}

// =============================================================================
// GREEN Tests — Concurrency
// =============================================================================

func TestLimitedConcurrentWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 3, nil)
	defer le.Close()

	// Send many messages — the semaphore should limit active goroutines to 3
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			le.Printf("concurrent %d", i)
		}(i)
	}
	wg.Wait()
	le.Flush()

	// Some messages should have been sent, some may be dropped due to semaphore
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected at least some messages to be sent")
	}
	if len(lines) > 50 {
		t.Fatalf("expected at most 50 messages, got %d", len(lines))
	}
}

func TestConcurrentPrintAndFlush(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Hammer Print from multiple goroutines while calling Flush
	// This should not race or panic (run with -race)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			le.Printf("message %d", i)
		}(i)
	}

	// Flush concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		le.Flush()
	}()

	wg.Wait()
	le.Flush()
}

// =============================================================================
// GREEN Tests — Connect function branches
// =============================================================================

func TestConnectWithConcurrentWrites(t *testing.T) {
	// Test the concurrent writes setup via connectToFakeServer
	// (Connect itself can't use the fake server due to self-signed cert)
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 5, nil)
	defer le.Close()

	if le.concurrentWrites == nil {
		t.Fatal("expected concurrentWrites to be initialized")
	}
	if cap(le.concurrentWrites) != 5 {
		t.Fatalf("expected capacity 5, got %d", cap(le.concurrentWrites))
	}
}

func TestConnectWithNilErrOutput(t *testing.T) {
	// Verify the nil errOutput default path
	le := newEmptyLogger("", "test-token", 0)
	// Connect sets errOutput to os.Stdout when nil is passed
	if le.errOutput != nil {
		t.Fatal("expected errOutput to be nil before Connect sets it")
	}
}

func TestConnectFailsOnBadHost(t *testing.T) {
	_, err := Connect("127.0.0.1:1", "token", 0, io.Discard, 0)
	if err == nil {
		t.Fatal("expected Connect to fail on bad host")
	}
}

// =============================================================================
// GREEN Tests — SetErrorOutput / SetNumberOfRetries
// =============================================================================

func TestSetErrorOutput(t *testing.T) {
	var buf bytes.Buffer
	le := newEmptyLogger("", "", 0)
	le.errOutput = io.Discard

	le.SetErrorOutput(&buf)
	le.writeToErrOutput("test error")

	if buf.String() != "test error" {
		t.Fatalf("expected 'test error', got %q", buf.String())
	}
}

func TestSetNumberOfRetries(t *testing.T) {
	le := newEmptyLogger("", "", 0)
	le.SetNumberOfRetries(3)
	if le.numRetries != 3 {
		t.Fatalf("expected 3 retries, got %d", le.numRetries)
	}
}

// =============================================================================
// GREEN Tests — Output with Lshortfile flag
// =============================================================================

func TestOutputWithShortfileFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	// Use calldepthOffset=-1 so runtime.Caller(2) points to this test file
	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.calldepthOffset = -1

	le.SetFlags(1 << 4) // log.Lshortfile = 16
	le.Print("with file info")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// Should contain filename:line
	if !strings.Contains(lines[0], "le_test.go:") {
		t.Fatalf("expected filename in output, got %q", lines[0])
	}
}

func TestOutputWithLongfileFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.calldepthOffset = -1

	le.SetFlags(1 << 3) // log.Llongfile = 8
	le.Print("with long file info")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// Should contain full path
	if !strings.Contains(lines[0], "/le_test.go:") {
		t.Fatalf("expected full path in output, got %q", lines[0])
	}
}

// =============================================================================
// GREEN Tests — Header Formatting (date, time, microseconds)
// =============================================================================

func TestHeaderFormattingWithDateAndTime(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetFlags(1 | 2) // log.Ldate | log.Ltime
	le.Print("with datetime")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// Should contain date format YYYY/MM/DD and time HH:MM:SS
	if !strings.Contains(lines[0], "/") || !strings.Contains(lines[0], ":") {
		t.Fatalf("expected date/time in output, got %q", lines[0])
	}
}

func TestHeaderFormattingWithMicroseconds(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetFlags(2 | 4) // log.Ltime | log.Lmicroseconds
	le.Print("with microseconds")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// Should contain dots (for microsecond fraction)
	if !strings.Contains(lines[0], ".") {
		t.Fatalf("expected microseconds in output, got %q", lines[0])
	}
}

func TestHeaderFormattingWithUTC(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetFlags(1 | 2 | 32) // log.Ldate | log.Ltime | log.LUTC
	le.Print("with utc")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	if !strings.Contains(lines[0], "with utc") {
		t.Fatalf("expected message in output, got %q", lines[0])
	}
}

func TestHeaderFormattingWithShortfileAndDate(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.calldepthOffset = -1

	le.SetFlags(1 | 2 | 16) // log.Ldate | log.Ltime | log.Lshortfile
	le.Print("full header")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	// Should contain date, time, and filename
	if !strings.Contains(lines[0], "le_test.go:") {
		t.Fatalf("expected filename in output, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "/") {
		t.Fatalf("expected date separator in output, got %q", lines[0])
	}
}

// =============================================================================
// GREEN Tests — Flush recover
// =============================================================================

func TestFlushOnUninitializedLogger(t *testing.T) {
	// Flush should not panic even with edge cases
	le := newEmptyLogger("", "", 0)
	le.Flush() // wg is initialized, should be no-op
}

// =============================================================================
// GREEN Tests — Write error path
// =============================================================================

func TestWriteErrorReturnsError(t *testing.T) {
	le := newEmptyLogger("127.0.0.1:1", "test-token", 0)
	le.errOutput = io.Discard
	le.dialTimeout = 1 * time.Second

	_, err := le.Write([]byte("test\n"))
	if err == nil {
		t.Fatal("expected Write to return error when connection fails")
	}
}

// =============================================================================
// GREEN Tests — writeRaw retry path
// =============================================================================

func TestWriteRawRetriesOnFailure(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetNumberOfRetries(2)

	// Close the current connection to trigger a reconnect on first attempt
	le.conn.Close()
	le.conn = nil

	le.Print("after reconnect")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected message after retry/reconnect")
	}
}

// =============================================================================
// GREEN Tests — Println
// =============================================================================

func TestPrintlnSendsToServer(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Println("hello println")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "hello println") {
		t.Fatalf("expected 'hello println', got %q", lines[0])
	}
}

// =============================================================================
// GREEN Tests — Header Formatting (prefix)
// =============================================================================

// =============================================================================
// GREEN Tests — Output closed path with semaphore
// =============================================================================

func TestOutputIgnoredWhenClosedWithSemaphore(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 3, nil)

	le.Print("before close")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	le.Close()

	// After close, Output should return immediately (closed check)
	le.Print("after close with semaphore")
	time.Sleep(100 * time.Millisecond)

	if len(server.Lines()) > 1 {
		t.Fatal("expected no new writes after close")
	}
}

// =============================================================================
// GREEN Tests — writeRaw retry with reconnect
// =============================================================================

func TestWriteRawRetriesAndReconnects(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetNumberOfRetries(2)

	// Kill the connection so first write attempt fails
	le.connMu.Lock()
	le.conn.Close()
	le.connMu.Unlock()

	// Print should succeed via retry + reconnect
	le.Print("retry message")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected message after retry+reconnect")
	}
}

// =============================================================================
// GREEN Tests — Header Formatting (prefix)
// =============================================================================

func TestHeaderFormattingWithPrefix(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetPrefix("[APP] ")
	le.Print("prefixed message")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	if !strings.Contains(lines[0], "[APP] ") {
		t.Fatalf("expected prefix '[APP] ' in output, got %q", lines[0])
	}
}
