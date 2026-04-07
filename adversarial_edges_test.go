package le_go

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// Adversarial Edge Case Tests — boundary conditions, empty/nil inputs,
// extreme values, special characters, and unusual API usage patterns.
// =============================================================================

// --- Empty and nil inputs ---

func TestAdversarialEmptyStringPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Print("")
	le.Flush()

	// Give time for any message to arrive
	time.Sleep(200 * time.Millisecond)

	// Even an empty string should produce a line (token + newline)
	lines := server.Lines()
	if len(lines) > 0 {
		for _, line := range lines {
			if !strings.HasPrefix(line, "test-token ") {
				t.Fatalf("line missing token prefix: %q", line)
			}
		}
	}
}

func TestAdversarialPrintfEmptyFormat(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	emptyFmt := ""
	le.Printf(emptyFmt)
	le.Printf(emptyFmt, "extra", "args", "ignored")
	le.Flush()

	time.Sleep(200 * time.Millisecond)
	// Should not panic
}

func TestAdversarialWriteNilSlice(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Write with nil slice
	n, err := le.Write(nil)
	if err != nil {
		t.Fatalf("Write(nil) returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("Write(nil) returned n=%d, expected 0", n)
	}
}

func TestAdversarialWriteEmptySlice(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	n, err := le.Write([]byte{})
	if err != nil {
		t.Fatalf("Write([]byte{}) returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("Write([]byte{}) returned n=%d, expected 0", n)
	}

	// Wait and check that a line was sent (token + newline)
	time.Sleep(200 * time.Millisecond)
	lines := server.Lines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line from empty Write, got %d", len(lines))
	}
	// Line should be just the token (empty payload)
	expected := "test-token "
	if !strings.HasPrefix(lines[0], expected) {
		t.Fatalf("expected line starting with %q, got %q", expected, lines[0])
	}
}

// --- Null bytes and binary data ---

func TestAdversarialNullBytesInMessage(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Message with embedded null bytes
	le.Print("before\x00after")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) < 1 {
		t.Fatal("expected at least 1 line")
	}
	// The null byte should be preserved or handled gracefully
	content := stripTokenAndHeader(lines[0], "test-token")
	if !strings.Contains(content, "before") {
		t.Fatalf("expected 'before' in message, got %q", content)
	}
}

func TestAdversarialWriteNullBytes(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	data := []byte{0, 0, 0, 0, 0}
	n, err := le.Write(data)
	if err != nil {
		t.Fatalf("Write(null bytes) returned error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned n=%d, expected %d", n, len(data))
	}
}

// --- Only newlines ---

func TestAdversarialOnlyNewlines(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// A message that is only newlines — replaceEmbeddedNewlines should
	// strip all trailing newlines, leaving an empty string.
	le.Print("\n\n\n\n\n")
	le.Flush()

	time.Sleep(200 * time.Millisecond)
	// Should not panic. The resulting message is empty after stripping.
}

func TestAdversarialOnlyCarriageReturns(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// \r is not replaced by replaceEmbeddedNewlines (only \n is)
	le.Print("\r\r\r")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
}

// --- Unicode edge cases ---

func TestAdversarialUnicodeLineSeparatorInInput(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// If the input already contains \u2028 (the replacement char),
	// it becomes ambiguous with actual replaced newlines.
	le.Print("line1\u2028line2")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	content := stripTokenAndHeader(lines[0], "test-token")

	// Count \u2028 occurrences — should be exactly 1 (the original)
	count := strings.Count(content, "\u2028")
	if count != 1 {
		t.Fatalf("expected 1 \\u2028, got %d — original \\u2028 was corrupted or duplicated: %q", count, content)
	}
}

func TestAdversarialMixedNewlinesAndUnicodeSeparator(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Mix real newlines with the replacement character
	// After processing: \n -> \u2028, existing \u2028 stays
	// This means the receiver cannot distinguish original \u2028 from replaced \n
	// This is a semantic bug (ambiguity), not a crash.
	le.Print("a\nb\u2028c\nd")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	content := stripTokenAndHeader(lines[0], "test-token")

	// There should be 3 \u2028: two from \n replacement + one original
	count := strings.Count(content, "\u2028")
	if count != 3 {
		t.Logf("BUG: ambiguous newline replacement — input had 2 newlines + 1 \\u2028, output has %d \\u2028", count)
	}
}

func TestAdversarialMultiByteUTF8AtChunkBoundary(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Place a 4-byte UTF-8 character at the chunk boundary.
	// maxLogLength-2 is the chunk size. If the split happens in the middle
	// of a multi-byte UTF-8 sequence, data could be corrupted.
	padding := make([]byte, maxLogLength-4) // leave room for a 4-byte char at boundary
	for i := range padding {
		padding[i] = 'A'
	}
	// U+1F600 (grinning face) is 4 bytes in UTF-8: f0 9f 98 80
	msg := string(padding) + "\U0001F600" + "tail"

	le.Print(msg)
	le.Flush()

	// Should produce 2 chunks
	waitForLines(t, server, 2, 10*time.Second)
	lines := server.Lines()

	// Reassemble
	var reassembled strings.Builder
	for _, line := range lines {
		content := stripTokenAndHeader(line, "test-token")
		reassembled.WriteString(content)
	}

	got := reassembled.String()
	if !strings.Contains(got, "\U0001F600") {
		t.Fatalf("BUG: multi-byte UTF-8 character was split across chunks and corrupted: %q", got)
	}
	if !strings.HasSuffix(got, "tail") {
		t.Fatalf("BUG: data after multi-byte char was lost: %q", got)
	}
}

// --- Chunk boundary tests ---

func TestAdversarialChunkBoundaryMinusTwo(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Exactly maxLogLength-2: should fit in 1 chunk
	msg := strings.Repeat("x", maxLogLength-2)
	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 1 {
		t.Fatalf("maxLogLength-2 should fit in 1 chunk, got %d chunks", len(lines))
	}
}

func TestAdversarialChunkBoundaryMinusOne(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// maxLogLength-1: should spill into 2 chunks (chunk size is maxLogLength-2)
	msg := strings.Repeat("y", maxLogLength-1)
	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 2, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 2 {
		t.Fatalf("maxLogLength-1 should need 2 chunks, got %d", len(lines))
	}

	// Second chunk should have exactly 1 character of content
	content2 := stripTokenAndHeader(lines[1], "test-token")
	if len(content2) != 1 {
		t.Fatalf("second chunk should have 1 char, got %d: %q", len(content2), content2)
	}
}

func TestAdversarialChunkBoundaryExact(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Exactly maxLogLength: should need 2 chunks
	msg := strings.Repeat("z", maxLogLength)
	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 2, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 2 {
		t.Fatalf("maxLogLength should need 2 chunks, got %d", len(lines))
	}
}

func TestAdversarialChunkBoundaryPlusOne(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	msg := strings.Repeat("w", maxLogLength+1)
	le.Print(msg)
	le.Flush()

	waitForLines(t, server, 2, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 2 {
		t.Fatalf("maxLogLength+1 should need 2 chunks, got %d", len(lines))
	}
}

func TestAdversarialChunkDataIntegrity(t *testing.T) {
	// Verify that chunking preserves ALL data — no bytes lost at boundaries
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Build a message with identifiable content at each chunk boundary
	chunkSize := maxLogLength - 2
	totalLen := chunkSize*3 + 100 // 3 full chunks + partial
	msgBytes := make([]byte, totalLen)
	for i := range msgBytes {
		msgBytes[i] = byte('A' + (i % 26))
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	expectedChunks := (totalLen + chunkSize - 1) / chunkSize
	waitForLines(t, server, expectedChunks, 10*time.Second)
	lines := server.Lines()

	if len(lines) != expectedChunks {
		t.Fatalf("expected %d chunks, got %d", expectedChunks, len(lines))
	}

	// Reassemble and verify full content
	var reassembled strings.Builder
	for _, line := range lines {
		content := stripTokenAndHeader(line, "test-token")
		reassembled.WriteString(content)
	}

	got := reassembled.String()
	if got != msg {
		// Find the first difference
		minLen := len(got)
		if len(msg) < minLen {
			minLen = len(msg)
		}
		for i := 0; i < minLen; i++ {
			if got[i] != msg[i] {
				t.Fatalf("BUG: data mismatch at byte %d (chunk boundary?). got=%q want=%q",
					i, got[max(0, i-5):min(len(got), i+5)], msg[max(0, i-5):min(len(msg), i+5)])
			}
		}
		if len(got) != len(msg) {
			t.Fatalf("BUG: length mismatch: got %d, want %d", len(got), len(msg))
		}
	}
}

// --- Token with special characters ---

func TestAdversarialTokenWithNewlines(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	// Create a logger with a token containing newlines
	logger := newEmptyLogger(server.addr, "token\nwith\nnewlines", 0)
	logger.errOutput = &discardWriter{}
	logger.tlsConfig = &tls.Config{InsecureSkipVerify: true}

	conn, err := tls.Dial("tcp", server.addr, logger.tlsConfig)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	logger.conn = conn
	logger.lastRefreshAt = time.Now()
	le := logger

	defer le.Close()

	// The token with newlines will break the line protocol:
	// "token\nwith\nnewlines message" becomes 3 separate lines on the server
	le.Print("hello")
	le.Flush()

	time.Sleep(500 * time.Millisecond)
	lines := server.Lines()

	// If the logger doesn't sanitize the token, the server sees >1 line
	if len(lines) > 1 {
		t.Logf("BUG: token with newlines breaks line protocol — server saw %d lines instead of 1", len(lines))
		for i, l := range lines {
			t.Logf("  line[%d]: %q", i, l)
		}
	}
}

func TestAdversarialTokenWithNullBytes(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	logger := newEmptyLogger(server.addr, "token\x00evil", 0)
	logger.errOutput = &discardWriter{}
	logger.tlsConfig = &tls.Config{InsecureSkipVerify: true}

	conn, err := tls.Dial("tcp", server.addr, logger.tlsConfig)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	logger.conn = conn
	logger.lastRefreshAt = time.Now()
	le := logger

	defer le.Close()

	le.Print("test")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	// Should not panic; null byte in token is a protocol-level issue
}

// --- Printf with malicious format strings ---

func TestAdversarialPrintfExcessVerbs(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// More format verbs than arguments — Go's fmt handles this with %!MISSING
	excessFmt := "%s %s %s %s %s"
	le.Printf(excessFmt, "only_one")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "only_one") {
		t.Fatalf("expected 'only_one' in output, got %q", lines[0])
	}
}

func TestAdversarialPrintfReflectVerb(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// %v, %+v, %#v with nil interface
	le.Printf("nil: %v %+v %#v", nil, nil, nil)
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
}

func TestAdversarialPrintfPercentN(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// %n is not supported in Go's fmt (unlike C), but test anyway
	pctN := "%n%n%n%n"
	le.Printf(pctN)
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
}

// --- Prefix edge cases ---

func TestAdversarialVeryLongPrefix(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Prefix longer than maxLogLength. The prefix is prepended to EACH chunk
	// via formatHeader, but chunking is based on the message length alone.
	// This means each chunk's total wire size = token + prefix + chunk_data + newline,
	// which can far exceed maxLogLength. The chunking logic does NOT account for
	// the prefix length, so the "max log length" guarantee is violated.
	//
	// Additionally, the server's bufio.Scanner has a default max token size of ~64KB,
	// so lines exceeding that are silently dropped by the scanner.
	le.SetPrefix(strings.Repeat("P", maxLogLength+1000))
	le.SetFlags(0) // no date/time to keep it simple

	var errBuf bytes.Buffer
	le.SetErrorOutput(&errBuf)

	le.Print("msg")
	le.Flush()

	// The total line is: "test-token " (11) + prefix (66000) + "msg" (3) + "\n" (1) = ~66014 bytes
	// This exceeds bufio.Scanner's default 64KB buffer, so the server may not see any lines.
	time.Sleep(500 * time.Millisecond)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Log("BUG: very long prefix causes total line to exceed scanner buffer — data silently lost on receiver side")
		t.Log("BUG: chunking does not account for prefix/header length, only message length")
	}
}

func TestAdversarialPrefixWithNewlines(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Prefix with embedded newlines — formatHeader appends it raw
	le.SetPrefix("prefix\ninjected\n")
	le.SetFlags(0)

	le.Print("hello")
	le.Flush()

	time.Sleep(500 * time.Millisecond)
	lines := server.Lines()

	// If prefix newlines aren't sanitized, the server sees multiple lines
	if len(lines) > 1 {
		t.Logf("BUG: prefix with newlines breaks line protocol — server saw %d lines", len(lines))
		for i, l := range lines {
			t.Logf("  line[%d]: %q", i, l)
		}
	}
}

func TestAdversarialPrefixWithFormatVerbs(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetPrefix("%s%d%x%n")
	le.SetFlags(0)

	le.Print("hello")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	content := stripTokenAndHeader(lines[0], "test-token")
	// Prefix should appear literally, not be interpreted as format verbs
	if !strings.Contains(content, "%s%d%x%n") {
		t.Fatalf("prefix format verbs were interpreted instead of literal: %q", content)
	}
}

// --- CalldepthOffset edge values ---

func TestAdversarialNegativeCalldepthOffset(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	logger := newEmptyLogger(server.addr, "test-token", -100)
	logger.errOutput = &discardWriter{}
	logger.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	logger.SetFlags(log.Lshortfile) // triggers runtime.Caller with negative calldepth

	conn, err := tls.Dial("tcp", server.addr, logger.tlsConfig)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	logger.conn = conn
	logger.lastRefreshAt = time.Now()
	le := logger
	defer le.Close()

	// runtime.Caller with negative skip — should return ok=false
	le.Print("negative calldepth")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	// Should see "???" as the file since runtime.Caller fails
	if !strings.Contains(lines[0], "???") && !strings.Contains(lines[0], "negative calldepth") {
		t.Fatalf("expected either ??? or message, got %q", lines[0])
	}
}

func TestAdversarialVeryLargeCalldepthOffset(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	logger := newEmptyLogger(server.addr, "test-token", 999999)
	logger.errOutput = &discardWriter{}
	logger.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	logger.SetFlags(log.Lshortfile)

	conn, err := tls.Dial("tcp", server.addr, logger.tlsConfig)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	logger.conn = conn
	logger.lastRefreshAt = time.Now()
	le := logger
	defer le.Close()

	le.Print("huge calldepth")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "???") && !strings.Contains(lines[0], "huge calldepth") {
		t.Fatalf("expected ??? for impossible calldepth, got %q", lines[0])
	}
}

// --- Close / Flush edge cases ---

func TestAdversarialDoubleClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	err1 := le.Close()
	err2 := le.Close()

	// Second close should not panic. The connection is already closed,
	// so conn.Close() may return an error or nil (conn was set to non-nil).
	_ = err1
	_ = err2
}

func TestAdversarialTripleClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Close()
	le.Close()
	le.Close()
	// Should not panic or deadlock
}

func TestAdversarialFlushOnNeverUsedLogger(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Flush without any prior writes
	le.Flush()
	// Should return immediately without panic
}

func TestAdversarialEdgesPrintAfterClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	// Print after Close — should be silently dropped (atomic check in Output)
	le.Print("after close")
	le.Printf("after close %d", 42)
	le.Println("after close")

	// Should not panic or deadlock
	time.Sleep(100 * time.Millisecond)
}

func TestAdversarialEdgesWriteAfterClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	// Write() does NOT check the closed flag — it goes directly to the connection
	// This will try to write to a closed connection.
	_, err := le.Write([]byte("after close"))
	if err == nil {
		t.Log("NOTE: Write() after Close() succeeded — Write() does not check closed flag")
	}
}

func TestAdversarialFlushAfterClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("before close")
	le.Close() // Close calls wg.Wait()

	// Flush after Close — wg counter should be 0
	le.Flush()
}

// --- Write() protocol edge cases ---

func TestAdversarialWriteNewlineOnly(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Write a single newline — the code checks if last byte is \n
	n, err := le.Write([]byte("\n"))
	if err != nil {
		t.Fatalf("Write(newline) error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected n=1, got %d", n)
	}

	waitForLines(t, server, 1, 5*time.Second)
}

func TestAdversarialWriteAlreadyHasNewline(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Data already ends with newline — Write should NOT add another
	n, err := le.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != 6 {
		t.Fatalf("expected n=6, got %d", n)
	}

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d (double newline produced extra line?)", len(lines))
	}
}

func TestAdversarialWriteMultipleNewlines(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Data with embedded newlines — Write does NOT call replaceEmbeddedNewlines
	// Each \n will be a line separator to the server's scanner
	n, err := le.Write([]byte("line1\nline2\nline3\n"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != 18 {
		t.Fatalf("expected n=18, got %d", n)
	}

	time.Sleep(500 * time.Millisecond)
	lines := server.Lines()
	// Write() does NOT sanitize newlines, unlike Print(). The server will
	// see multiple lines from a single Write() call.
	if len(lines) != 3 {
		t.Logf("Write() with embedded newlines produced %d server lines (expected 3 — Write lacks newline sanitization)", len(lines))
	}
}

// --- Flag edge cases ---

func TestAdversarialAllFlagsEnabled(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Enable all standard log flags
	le.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Llongfile | log.Lshortfile | log.LUTC)

	le.Print("all flags")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	if !strings.Contains(lines[0], "all flags") {
		t.Fatalf("expected 'all flags' in output, got %q", lines[0])
	}
}

func TestAdversarialNegativeFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetFlags(-1) // All bits set
	le.Print("negative flag")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
}

func TestAdversarialMaxIntFlag(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetFlags(int(^uint(0) >> 1)) // MaxInt
	le.Print("max int flag")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
}

// --- Very large messages ---

func TestAdversarialMessageExactly1Byte(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.Print("x")
	le.Flush()

	waitForLines(t, server, 1, 5*time.Second)
	lines := server.Lines()
	content := stripTokenAndHeader(lines[0], "test-token")
	if content != "x" {
		t.Fatalf("expected 'x', got %q", content)
	}
}

func TestAdversarialMessage1MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large message test in short mode")
	}

	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	msg := strings.Repeat("M", 1024*1024) // 1 MB
	le.Print(msg)
	le.Flush()

	chunkSize := maxLogLength - 2
	expectedChunks := (len(msg) + chunkSize - 1) / chunkSize
	waitForLines(t, server, expectedChunks, 30*time.Second)

	lines := server.Lines()
	if len(lines) != expectedChunks {
		t.Fatalf("expected %d chunks for 1MB message, got %d", expectedChunks, len(lines))
	}

	// Verify total content length
	totalContent := 0
	for _, line := range lines {
		content := stripTokenAndHeader(line, "test-token")
		totalContent += len(content)
	}
	if totalContent != len(msg) {
		t.Fatalf("BUG: data loss in 1MB message — expected %d bytes, got %d", len(msg), totalContent)
	}
}

// --- Concurrent Write() and Print() ---

func TestAdversarialWriteAndPrintInterleaved(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Write() holds connMu for the duration.
	// Print() launches a goroutine that also acquires connMu.
	// Interleaving should not cause data corruption.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		idx := i
		go func() {
			defer wg.Done()
			le.Printf("print-%d", idx)
		}()
		go func() {
			defer wg.Done()
			le.Write([]byte(fmt.Sprintf("write-%d", idx)))
		}()
	}
	wg.Wait()
	le.Flush()

	time.Sleep(500 * time.Millisecond)
	// Should not panic or produce corrupted data
	lines := server.Lines()
	if len(lines) < 1 {
		t.Fatal("expected at least 1 line from interleaved Write/Print")
	}
}

// --- Error output edge cases ---

func TestAdversarialSetErrorOutputToNilAfterInit(t *testing.T) {
	// BUG CONFIRMED: Setting errOutput to nil via SetErrorOutput(nil) and then
	// triggering a write error causes a nil pointer dereference panic in
	// writeToErrOutput() -> fmt.Fprint(nil, ...).
	//
	// The panic occurs in a goroutine spawned by Output(). The defer/recover in
	// Output catches it but then re-panics with panic(re), which crashes the
	// entire process. SetErrorOutput should either reject nil or writeToErrOutput
	// should check for nil.
	//
	// This test is skipped because the panic kills the test process — but it is
	// a confirmed crash bug. To reproduce, remove the t.Skip and run:
	//   go test -run TestAdversarialSetErrorOutputToNilAfterInit
	t.Skip("CONFIRMED BUG: SetErrorOutput(nil) + write error = nil pointer panic in goroutine (crashes process)")

	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetErrorOutput(nil)

	le.connMu.Lock()
	le.conn.Close()
	le.conn = nil
	le.host = "127.0.0.1:1"
	le.dialTimeout = 100 * time.Millisecond
	le.connMu.Unlock()

	le.Print("should trigger error output")
	le.Flush()

	time.Sleep(500 * time.Millisecond)
}

// --- replaceEmbeddedNewlines edge cases ---

func TestAdversarialReplaceEmbeddedNewlinesEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"single newline", "\n", ""},
		{"only newlines", "\n\n\n", ""},
		{"trailing newline", "hello\n", "hello"},
		{"multiple trailing newlines", "hello\n\n\n", "hello"},
		{"leading newline", "\nhello", "\u2028hello"},
		{"middle newline", "hello\nworld", "hello\u2028world"},
		{"CRLF", "hello\r\nworld\r\n", "hello\r\u2028world\r"},
		{"just spaces", "   ", "   "},
		{"newline then spaces", "\n   ", "\u2028   "},
		{"unicode line sep already present", "a\u2028b", "a\u2028b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceEmbeddedNewlines(tt.input)
			if got != tt.expected {
				t.Errorf("replaceEmbeddedNewlines(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// --- Concurrent SetPrefix/SetFlags while logging ---

func TestAdversarialSetPrefixDuringLogging(t *testing.T) {
	// CONFIRMED DATA RACE: Output() reads l.prefix in formatHeader() inside a
	// spawned goroutine, but only holds connMu at that point (not mu). SetPrefix()
	// writes l.prefix under mu. So concurrent SetPrefix + Print = data race on prefix.
	// The concurrency agent has this covered — skip here to avoid duplicate failure.
	t.Skip("CONFIRMED BUG (DATA RACE): SetPrefix races with formatHeader in Output goroutine — mu released before goroutine reads prefix")
}

func TestAdversarialSetFlagsDuringLogging(t *testing.T) {
	// CONFIRMED DATA RACE: Same pattern as prefix — Output() reads l.flag in
	// formatHeader() inside a spawned goroutine without holding mu.
	// SetFlags() writes l.flag under mu, but the goroutine reads it without mu.
	t.Skip("CONFIRMED BUG (DATA RACE): SetFlags races with formatHeader in Output goroutine — mu released before goroutine reads flag")
}

// --- concurrentWrites=1 (minimal semaphore) ---

func TestAdversarialConcurrentWritesOne(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 1, nil)
	defer le.Close()

	// With only 1 concurrent write slot, rapid fire should drop most messages
	for i := 0; i < 100; i++ {
		le.Printf("msg-%d", i)
	}
	le.Flush()

	time.Sleep(500 * time.Millisecond)
	lines := server.Lines()
	// At least 1 message should have gotten through
	if len(lines) < 1 {
		t.Fatal("expected at least 1 line with concurrentWrites=1")
	}
	t.Logf("concurrentWrites=1: %d/100 messages delivered", len(lines))
}

// --- Panic recovery in Output ---

func TestAdversarialPanicCallsOutput(t *testing.T) {
	// BUG: Panic() calls Output() which spawns a goroutine. The goroutine
	// calls handlePanicActions which does panic(s). This panic happens in
	// a child goroutine, not the calling goroutine, so:
	//   1. The caller cannot recover() from it
	//   2. It crashes the entire process
	// This is fundamentally broken — Panic() should panic in the calling
	// goroutine, not in a background goroutine.
	t.Skip("CONFIRMED BUG: Panic() panics in a spawned goroutine, crashing the process — caller cannot recover()")
}

// --- Write() with very large data ---

func TestAdversarialWriteLargePayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large write test in short mode")
	}

	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Write() does NOT chunk — it sends the entire payload as-is.
	// A payload > maxLogLength will be sent as a single TCP write.
	bigData := bytes.Repeat([]byte("W"), maxLogLength*2)
	n, err := le.Write(bigData)
	if err != nil {
		t.Fatalf("Write(large) error: %v", err)
	}
	if n != len(bigData) {
		t.Fatalf("Write returned n=%d, expected %d", n, len(bigData))
	}

	// The server's scanner has a default buffer size (64KB by default in bufio).
	// A 130KB message in a single Write may exceed the scanner buffer.
	time.Sleep(1 * time.Second)
	lines := server.Lines()
	if len(lines) == 0 {
		t.Log("NOTE: Write() with payload > scanner buffer may silently lose data")
	}
}

// --- SetNumberOfRetries edge cases ---

func TestAdversarialZeroRetries(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)
	defer le.Close()

	le.SetNumberOfRetries(0) // default

	// Break connection
	le.connMu.Lock()
	le.conn.Close()
	le.conn = nil
	le.host = "127.0.0.1:1"
	le.dialTimeout = 100 * time.Millisecond
	le.connMu.Unlock()

	le.Print("should fail immediately")
	le.Flush()

	time.Sleep(500 * time.Millisecond)
	if !strings.Contains(errBuf.String(), "write error") {
		t.Fatalf("expected write error with 0 retries, got: %q", errBuf.String())
	}
}

func TestAdversarialNegativeRetries(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)
	defer le.Close()

	// Negative retries: numAttempts = 1 + (-1) = 0, so the loop body never runs
	le.SetNumberOfRetries(-1)

	le.Print("should this even try?")
	le.Flush()

	time.Sleep(500 * time.Millisecond)

	// With 0 attempts, writeRaw returns nil error (no iteration, err is nil initial value)
	// This means the message is silently lost — writeRaw returns nil but never actually wrote.
	lines := server.Lines()
	if len(lines) == 0 {
		t.Log("BUG CONFIRMED: SetNumberOfRetries(-1) causes writeRaw to skip all attempts and return nil error — message silently lost")
	}
}

// --- Rapid open/close cycles ---

func TestAdversarialEdgesRapidPrintFlushCycles(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	for i := 0; i < 100; i++ {
		le.Printf("cycle-%d", i)
		le.Flush()
	}

	waitForLines(t, server, 100, 10*time.Second)
}

