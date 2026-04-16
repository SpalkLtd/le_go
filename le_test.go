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
// Connection Management
// =============================================================================

func TestConnectOpensConnection(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.withConnLocked(func(c net.Conn) {
		if c == nil {
			t.Fatal("expected connection to be open")
		}
	})
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

	le.withConnLocked(func(c net.Conn) {
		if c != nil {
			t.Fatal("expected le.conn to be nil after Close")
		}
	})
}

func TestConnectFailsOnBadHost(t *testing.T) {
	_, err := Connect("127.0.0.1:1", "token", 0, io.Discard, 0, WithDialTimeout(1*time.Second))
	if err == nil {
		t.Fatal("expected Connect to fail on bad host")
	}
}

// =============================================================================
// Configuration
// =============================================================================

func TestFlagsReturnsFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()
	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetFlags(2)

	if le.Flags() != 2 {
		t.Fatalf("expected flag 2, got %d", le.Flags())
	}
}

func TestSetFlagsSetsFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()
	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetFlags(1)

	if le.Flags() != 1 {
		t.Fatalf("expected flag 1, got %d", le.Flags())
	}
}

func TestPrefixReturnsPrefix(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()
	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetPrefix("myPrefix")

	if le.Prefix() != "myPrefix" {
		t.Fatalf("expected prefix 'myPrefix', got %q", le.Prefix())
	}
}

func TestSetPrefixSetsPrefix(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()
	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetPrefix("myNewPrefix")

	if le.Prefix() != "myNewPrefix" {
		t.Fatalf("expected prefix 'myNewPrefix', got %q", le.Prefix())
	}
}

// =============================================================================
// Core Logging
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

func TestEmptyMessageHandling(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Print("")
	le.Flush()
}

// =============================================================================
// Newline Replacement
// =============================================================================

func TestReplaceNewline(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Println("1\n2\n3")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if strings.Count(lines[0], "\u2028") < 2 {
		t.Fatalf("expected at least 2 \\u2028 replacements, got %q", lines[0])
	}
}

func TestNewlineReplacementEmbeddedOnly(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Print("hello\nworld")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) > 1 {
		t.Fatalf("expected 1 line (embedded \\n replaced), got %d lines", len(lines))
	}
	if len(lines) == 1 && !strings.Contains(lines[0], "hello\u2028world") {
		t.Fatalf("expected 'hello\\u2028world' in line, got %q", lines[0])
	}
}

func TestAddNewlineWhenMissing(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Print("hello")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) < 1 {
		t.Fatal("expected at least 1 line")
	}
}

// =============================================================================
// io.Writer Interface
// =============================================================================

func TestLoggerImplementsWriterInterface(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	func(w io.Writer) {}(le)
}

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
	if n != len(input) {
		t.Fatalf("expected n=%d, got %d", len(input), n)
	}
}

func TestWriteReturnsErrLoggerClosed(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	_, err := le.Write([]byte("after close\n"))
	if err != ErrLoggerClosed {
		t.Fatalf("expected ErrLoggerClosed, got %v", err)
	}
}

func TestWriteDoesNotBlockBehindPrintQueue(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 5, nil)
	defer le.Close()

	for i := 0; i < 100; i++ {
		le.Printf("fill %d", i)
	}

	done := make(chan struct{})
	go func() {
		le.Write([]byte("direct write\n"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked behind Print queue")
	}
}

func TestWriteErrorReturnsError(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, &discardWriter{})
	defer le.Close()

	le.connMu.Lock()
	if le.conn != nil {
		le.conn.Close()
	}
	le.conn = nil
	le.host = "127.0.0.1:1"
	le.dialTimeout = 1 * time.Second
	le.connMu.Unlock()

	_, err := le.Write([]byte("test\n"))
	if err == nil {
		t.Fatal("expected Write to return error when connection fails")
	}
}

// =============================================================================
// Flush
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

	waitForLines(t, server, 10, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 10 {
		t.Fatalf("expected 10 lines after flush, got %d", len(lines))
	}
}

func TestFlushReturnsErrorAfterClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	err := le.Flush()
	if err != ErrLoggerClosed {
		t.Fatalf("expected ErrLoggerClosed, got %v", err)
	}
}

func TestFlushOnFreshLogger(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	err := le.Flush()
	if err != nil {
		t.Fatalf("expected nil error from Flush on fresh logger, got %v", err)
	}
}

// =============================================================================
// Close
// =============================================================================

func TestCloseWaitsForInFlightWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	server.SetReadDelay(50 * time.Millisecond)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("important message")
	le.Close()

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

	le.Print("after close")
	time.Sleep(200 * time.Millisecond)

	linesAfter := len(server.Lines())
	if linesAfter > linesBefore {
		t.Fatal("expected no new lines after Close(), but writes were still accepted")
	}
}

func TestCloseIdempotent(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	err1 := le.Close()
	err2 := le.Close()
	if err1 != nil || err2 != nil {
		t.Fatalf("expected nil from both Close calls, got %v and %v", err1, err2)
	}
}

func TestOutputIgnoredWhenClosed(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 3, nil)

	le.Print("before close")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	le.Close()

	le.Print("after close with small channel")
	time.Sleep(100 * time.Millisecond)

	if len(server.Lines()) > 1 {
		t.Fatal("expected no new writes after close")
	}
}

// =============================================================================
// Error Output
// =============================================================================

func TestWriteFailureLogsToErrOutput(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)

	le.connMu.Lock()
	if le.conn != nil {
		le.conn.Close()
	}
	le.conn = nil
	le.host = "127.0.0.1:1"
	le.dialTimeout = 1 * time.Second
	le.connMu.Unlock()

	le.Print("test message")
	le.Flush()
	time.Sleep(500 * time.Millisecond)

	errOutput := errBuf.String()
	if errOutput == "" {
		t.Fatal("expected error output, got nothing")
	}
}

func TestSetErrorOutput(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, &discardWriter{})
	defer le.Close()

	var buf bytes.Buffer
	le.SetErrorOutput(&buf)
	le.writeToErrOutput("test error")

	if buf.String() != "test error" {
		t.Fatalf("expected 'test error', got %q", buf.String())
	}
}

func TestWriteToErrOutputWithPercent(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, &discardWriter{})
	defer le.Close()

	var errBuf bytes.Buffer
	le.SetErrorOutput(&errBuf)

	le.writeToErrOutput("100% complete %s %d")

	errOutput := errBuf.String()
	if strings.Contains(errOutput, "%!") {
		t.Fatalf("errOutput contains format verb artifacts: %q", errOutput)
	}
	if errOutput != "100% complete %s %d" {
		t.Fatalf("expected exact string, got %q", errOutput)
	}
}

func TestSetNumberOfRetries(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetNumberOfRetries(3)
	le.cfgMu.Lock()
	retries := le.numRetries
	le.cfgMu.Unlock()
	if retries != 3 {
		t.Fatalf("expected 3 retries, got %d", retries)
	}
}

// =============================================================================
// Connection Refresh / Connection Storm Bug Fix
// =============================================================================

func TestLastRefreshAtUpdatedOnReconnect(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.connMu.Lock()
	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
	le.connMu.Unlock()

	le.Print("after stale")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)
	waitForConnections(t, server, 2, 5*time.Second)

	le.Print("second write")
	le.Flush()
	waitForLines(t, server, 2, 5*time.Second)

	if server.ConnectionCount() > 2 {
		t.Fatalf("expected 2 connections (initial + 1 reconnect), got %d", server.ConnectionCount())
	}
}

func TestConnectionRefreshDoesNotReconnectEveryWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.connMu.Lock()
	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
	le.connMu.Unlock()

	for i := 0; i < 5; i++ {
		le.Printf("msg %d", i)
		le.Flush()
	}
	waitForLines(t, server, 5, 5*time.Second)

	if server.ConnectionCount() > 2 {
		t.Fatalf("expected 2 connections, got %d", server.ConnectionCount())
	}
}

func TestOldConnectionClosedOnReconnect(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var initialConn net.Conn
	le.withConnLocked(func(c net.Conn) {
		initialConn = c
	})

	le.connMu.Lock()
	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
	le.connMu.Unlock()

	le.Print("trigger reconnect")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	_, err := initialConn.Write([]byte("test"))
	if err == nil {
		t.Fatal("expected initial connection to be closed after reconnect")
	}
}

// =============================================================================
// Chunking
// =============================================================================

func TestChunkingContentCorrectness(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	msgBytes := make([]byte, 140000)
	for i := range msgBytes {
		msgBytes[i] = byte('a' + (i % 26))
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 2, 10*time.Second)
	lines := server.Lines()

	var reassembled strings.Builder
	for _, line := range lines {
		content := stripTokenAndHeader(line, "test-token")
		reassembled.WriteString(content)
	}

	got := reassembled.String()
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

	msgBytes := make([]byte, maxLogLength-2)
	for i := range msgBytes {
		msgBytes[i] = 'x'
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()

	if len(lines) != 1 {
		t.Fatalf("expected 1 line for exact boundary message, got %d", len(lines))
	}

	content := stripTokenAndHeader(lines[0], "test-token")
	if len(content) < len(msg) {
		t.Fatalf("expected content length >= %d, got %d", len(msg), len(content))
	}
}

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

	for i, line := range lines {
		if !strings.HasPrefix(line, "test-token ") {
			t.Fatalf("chunk %d missing token prefix: %q", i, line[:50])
		}
	}
}

func TestEachChunkNewlineTerminated(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	msgBytes := make([]byte, maxLogLength*2+1000)
	for i := range msgBytes {
		msgBytes[i] = byte('a' + (i % 26))
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 3, 10*time.Second)
	lines := server.Lines()
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 separate lines (one per chunk), got %d", len(lines))
	}
}

// =============================================================================
// Header Formatting
// =============================================================================

func TestOutputWithShortfileFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.calldepthOffset = -1

	le.SetFlags(1 << 4) // log.Lshortfile
	le.Print("with file info")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
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

	le.SetFlags(1 << 3) // log.Llongfile
	le.Print("with long file info")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "/le_test.go:") {
		t.Fatalf("expected full path in output, got %q", lines[0])
	}
}

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
	if !strings.Contains(lines[0], "le_test.go:") {
		t.Fatalf("expected filename in output, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "/") {
		t.Fatalf("expected date separator in output, got %q", lines[0])
	}
}

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

// =============================================================================
// Dial Timeout
// =============================================================================

func TestDialTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	done := make(chan error, 1)
	go func() {
		_, err := Connect(addr, "token", 0, io.Discard, 0, WithDialTimeout(2*time.Second))
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Connect hung for >15s — no dial timeout")
	}
}

// =============================================================================
// Retry
// =============================================================================

func TestWriteRawRetriesOnFailure(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetNumberOfRetries(2)

	le.connMu.Lock()
	if le.conn != nil {
		le.conn.Close()
	}
	le.conn = nil
	le.connMu.Unlock()

	le.Print("after reconnect")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected message after retry/reconnect")
	}
}

func TestWriteRawRetriesAndReconnects(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetNumberOfRetries(2)

	le.connMu.Lock()
	if le.conn != nil {
		le.conn.Close()
	}
	le.conn = nil
	le.connMu.Unlock()

	le.Print("retry message")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected message after retry+reconnect")
	}
}

// =============================================================================
// Pipeline — Lifecycle & Goroutine Management
// =============================================================================

func TestPipeline_LifecycleStartupShutdown(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	select {
	case <-le.exited:
	default:
		t.Fatal("expected exited channel to be closed after Close")
	}
}

func TestPipeline_BoundedGoroutines(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 100, nil)
	defer le.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			le.Printf("msg %d", i)
		}(i)
	}
	wg.Wait()
	le.Flush()
}

func TestPipeline_DropCounter(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.SetReadDelay(100 * time.Millisecond)
	le := connectToFakeServer(t, server, 2, nil)
	defer le.Close()

	for i := 0; i < 100; i++ {
		le.Printf("message %d", i)
	}
	le.Flush()

	stats := le.Stats()
	if stats.DroppedQueueFull == 0 {
		t.Fatal("expected some drops due to queue full, got 0")
	}
}

func TestPipeline_DropCounterClosed(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	le.Print("after close")
	le.Print("after close 2")

	stats := le.Stats()
	if stats.DroppedClosed == 0 {
		t.Fatal("expected DroppedClosed > 0, got 0")
	}
}

func TestPipeline_FIFOOrdering(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	for i := 0; i < 50; i++ {
		le.Printf("seq-%03d", i)
	}
	le.Flush()

	waitForLines(t, server, 50, 10*time.Second)
	lines := server.Lines()

	for i := 0; i < len(lines); i++ {
		expected := fmt.Sprintf("seq-%03d", i)
		if !strings.Contains(lines[i], expected) {
			t.Fatalf("FIFO violation at index %d: expected %q, got %q", i, expected, lines[i])
		}
	}
}

func TestPipeline_CloseDuringInFlight(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	for i := 0; i < 20; i++ {
		le.Printf("inflight-%d", i)
	}
	le.Close()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected at least some messages to be drained on Close")
	}
}

// =============================================================================
// Pipeline — Drop via test hook
// =============================================================================

func TestPipeline_DropHookFires(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	var mu sync.Mutex
	drops := make(map[DropReason]int)

	server.SetReadDelay(100 * time.Millisecond)
	le := connectToFakeServer(t, server, 2, nil, WithTestDropped(func(r DropReason) {
		mu.Lock()
		drops[r]++
		mu.Unlock()
	}))
	defer le.Close()

	for i := 0; i < 100; i++ {
		le.Printf("msg %d", i)
	}
	le.Flush()

	mu.Lock()
	queueFull := drops[DropQueueFull]
	mu.Unlock()

	if queueFull == 0 {
		t.Fatal("expected DropQueueFull hook to fire, got 0")
	}
}

// =============================================================================
// Concurrency
// =============================================================================

func TestConcurrentPrintAndFlush(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			le.Printf("message %d", i)
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		le.Flush()
	}()

	wg.Wait()
	le.Flush()
}

func TestLimitedConcurrentWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 3, nil)
	defer le.Close()

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

	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected at least some messages to be sent")
	}
	if len(lines) > 50 {
		t.Fatalf("expected at most 50 messages, got %d", len(lines))
	}
}

func TestWriteConcurrentWithPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			le.Printf("print %d", i)
		}(i)
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			le.Write([]byte(fmt.Sprintf("write %d\n", i)))
		}(i)
	}

	wg.Wait()
	le.Flush()

	waitForLines(t, server, 20, 10*time.Second)
}

// =============================================================================
// Backpressure / Drops
// =============================================================================

func TestDroppedLogsUnderBackpressure(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.SetReadDelay(100 * time.Millisecond)
	le := connectToFakeServer(t, server, 2, nil)
	defer le.Close()

	for i := 0; i < 100; i++ {
		le.Printf("message %d", i)
	}
	le.Flush()

	lines := server.Lines()
	if len(lines) > 100 {
		t.Fatalf("expected at most 100 lines, got %d", len(lines))
	}
}

// =============================================================================
// Stats
// =============================================================================

func TestStats_AllCounters(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.SetReadDelay(100 * time.Millisecond)
	le := connectToFakeServer(t, server, 2, nil)

	for i := 0; i < 50; i++ {
		le.Printf("fill %d", i)
	}
	le.Flush()

	stats := le.Stats()
	if stats.DroppedQueueFull == 0 {
		t.Fatal("expected DroppedQueueFull > 0")
	}

	le.Close()

	le.Print("after close")
	stats = le.Stats()
	if stats.DroppedClosed == 0 {
		t.Fatal("expected DroppedClosed > 0")
	}
}

// =============================================================================
// Panic recovery
// =============================================================================

func TestPanic_RecoverableByCaller(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	recovered := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				recovered = true
				if s, ok := r.(string); !ok || !strings.Contains(s, "test panic") {
					t.Fatalf("expected panic with 'test panic', got %v", r)
				}
			}
		}()
		le.Panic("test panic")
	}()

	if !recovered {
		t.Fatal("expected Panic to be recoverable in caller")
	}
}

func TestPanic_MessageWrittenBeforePanic(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	func() {
		defer func() { recover() }()
		le.Panic("panic msg")
	}()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Fatal("expected panic message to be written before panic")
	}
	if !strings.Contains(lines[0], "panic msg") {
		t.Fatalf("expected panic message in output, got %q", lines[0])
	}
}

// =============================================================================
// Connect branches
// =============================================================================

func TestConnectWithNilErrOutput(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	if le.errOutput == nil {
		t.Fatal("expected errOutput to be non-nil")
	}
}
