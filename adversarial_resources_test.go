package le_go

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// Adversarial Resource Exhaustion Tests
// =============================================================================

// TestAdversarialSemaphoreStarvationAndRecovery fills all semaphore tokens
// and verifies the logger recovers (tokens are returned) after writes complete.
func TestAdversarialSemaphoreStarvationAndRecovery(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	const semSize = 2
	le := connectToFakeServer(t, server, semSize, nil)
	defer le.Close()

	// Slow down the server so writes take time and tokens stay consumed.
	server.SetReadDelay(50 * time.Millisecond)

	// Fire a burst that should saturate the semaphore.
	for i := 0; i < 20; i++ {
		le.Printf("burst-%d", i)
	}

	// Flush to wait for all in-flight writes.
	le.Flush()

	// After flush, ALL semaphore tokens must be returned.
	available := len(le.concurrentWrites)
	if available != semSize {
		t.Fatalf("expected %d semaphore tokens after flush, got %d (token leak)", semSize, available)
	}

	// Remove read delay for the recovery phase so the server reads promptly.
	server.SetReadDelay(0)

	// Now verify the logger is still functional — tokens recovered.
	le.Print("recovery-message")
	le.Flush()

	// Wait for burst lines + recovery line to arrive at the server.
	waitForLines(t, server, 3, 5*time.Second)
	found := false
	for _, line := range server.Lines() {
		if strings.Contains(line, "recovery-message") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("logger did not recover after semaphore starvation — recovery message not received")
	}
}

// TestAdversarialSemaphoreTokenLeakOnWriteError verifies semaphore tokens are
// returned even when the underlying write fails (connection broken).
func TestAdversarialSemaphoreTokenLeakOnWriteError(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	const semSize = 3
	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, semSize, &errBuf)
	defer le.Close()

	// Break the connection so writes fail.
	le.conn.Close()
	le.conn = nil
	le.host = "127.0.0.1:1" // unreachable
	le.dialTimeout = 500 * time.Millisecond

	// Attempt writes that will fail.
	for i := 0; i < 10; i++ {
		le.Printf("will-fail-%d", i)
	}
	le.Flush()

	// Even though writes failed, tokens must be returned.
	available := len(le.concurrentWrites)
	if available != semSize {
		t.Fatalf("expected %d semaphore tokens after failed writes, got %d (leak on error path)", semSize, available)
	}
}

// TestAdversarialGoroutineAccumulation spawns many writes and verifies all
// goroutines are cleaned up after Flush.
func TestAdversarialGoroutineAccumulation(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil) // unlimited concurrency
	defer le.Close()

	// Let GC and runtime settle.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const numWrites = 500
	for i := 0; i < numWrites; i++ {
		le.Printf("goroutine-test-%d", i)
	}
	le.Flush()

	// Wait for server to process everything.
	waitForLines(t, server, numWrites, 30*time.Second)

	// Let goroutines wind down.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - baseline
	// Allow a small margin (test infrastructure goroutines).
	if leaked > 10 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d, leaked=%d", baseline, after, leaked)
	}
}

// TestAdversarialBufMemoryGrowthAfterLargeMessage sends a huge message followed
// by many small ones, checking if the internal buf's backing array ever shrinks.
// This is an observation test — it documents the behavior.
func TestAdversarialBufMemoryGrowthAfterLargeMessage(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Send a large message (~1MB) that will require many chunks.
	bigMsg := strings.Repeat("X", 1_000_000)
	le.Print(bigMsg)
	le.Flush()

	// Wait for the large message chunks to arrive.
	// 1M / (65000-2) = ~16 chunks
	waitForLines(t, server, 15, 30*time.Second)

	// Record buf capacity after the large message.
	le.connMu.Lock()
	capAfterBig := cap(le.buf)
	le.connMu.Unlock()

	// Now send 100 small messages.
	for i := 0; i < 100; i++ {
		le.Printf("small-%d", i)
	}
	le.Flush()

	// Check buf capacity — it should still be large (buf[:0] doesn't shrink).
	le.connMu.Lock()
	capAfterSmall := cap(le.buf)
	le.connMu.Unlock()

	// Document the behavior: buf never shrinks. This is a potential memory issue.
	if capAfterSmall < capAfterBig {
		t.Logf("buf DID shrink: %d -> %d (unexpected but good)", capAfterBig, capAfterSmall)
	} else {
		t.Logf("buf did NOT shrink after large message: cap=%d bytes retained (potential memory waste)", capAfterSmall)
		// This is the expected (bad) behavior — the buffer retains its peak allocation.
		// Not a hard failure, but worth documenting.
		if capAfterSmall > 100_000 {
			t.Logf("WARNING: buf retains %d bytes after only needing ~100 bytes per message", capAfterSmall)
		}
	}
}

// TestAdversarialRapidPrintFlushCycles hammers Print+Flush in a tight loop.
func TestAdversarialRapidPrintFlushCycles(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	const iterations = 2000
	for i := 0; i < iterations; i++ {
		le.Printf("rapid-%d", i)
		le.Flush()
	}

	// All messages should arrive (no concurrency limit).
	waitForLines(t, server, iterations, 60*time.Second)
	lines := server.Lines()
	if len(lines) < iterations {
		t.Fatalf("expected %d lines, got %d (messages lost in rapid Print+Flush)", iterations, len(lines))
	}
}

// TestAdversarialManyWritersTinySemaphore tests 200 concurrent writers
// fighting over a semaphore of size 2.
func TestAdversarialManyWritersTinySemaphore(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	const semSize = 2
	le := connectToFakeServer(t, server, semSize, nil)
	defer le.Close()

	var wg sync.WaitGroup
	const numWriters = 200

	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			le.Printf("writer-%d", n)
		}(i)
	}
	wg.Wait()
	le.Flush()

	// All tokens must be returned.
	available := len(le.concurrentWrites)
	if available != semSize {
		t.Fatalf("expected %d tokens after %d concurrent writers, got %d", semSize, numWriters, available)
	}

	// Some messages will be dropped (semaphore full), but no panics or hangs.
	lines := server.Lines()
	t.Logf("received %d/%d messages (dropped %d due to backpressure)", len(lines), numWriters, numWriters-len(lines))
}

// TestAdversarialWriteTightLoop calls Write() (the io.Writer interface)
// thousands of times in a tight loop.
func TestAdversarialWriteTightLoop(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	const iterations = 3000
	for i := 0; i < iterations; i++ {
		msg := fmt.Sprintf("write-loop-%d\n", i)
		_, err := le.Write([]byte(msg))
		if err != nil {
			t.Fatalf("Write() failed at iteration %d: %v", i, err)
		}
	}

	waitForLines(t, server, iterations, 60*time.Second)
	lines := server.Lines()
	if len(lines) < iterations {
		t.Fatalf("expected %d lines, got %d", iterations, len(lines))
	}
}

// TestAdversarialConcurrentFlush calls Flush() from many goroutines
// simultaneously while writes are in progress.
func TestAdversarialConcurrentFlush(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Start some writes.
	for i := 0; i < 50; i++ {
		le.Printf("concurrent-flush-%d", i)
	}

	// Flush from 20 goroutines concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			le.Flush() // should not panic (has recover)
		}()
	}
	wg.Wait()

	// Verify messages arrived.
	waitForLines(t, server, 50, 10*time.Second)
}

// TestAdversarialLargeMessageChunking sends a ~2MB message and verifies
// all chunks arrive with correct content.
func TestAdversarialLargeMessageChunking(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// ~2MB message — will require ~31 chunks.
	msgSize := 2_000_000
	msgBytes := make([]byte, msgSize)
	for i := range msgBytes {
		msgBytes[i] = byte('a' + (i % 26))
	}
	msg := string(msgBytes)

	le.Print(msg)
	le.Flush()

	// Expected chunks: ceil(2000000 / (65000-2)) = 31
	expectedChunks := (msgSize + maxLogLength - 3) / (maxLogLength - 2)
	waitForLines(t, server, expectedChunks, 30*time.Second)

	lines := server.Lines()
	if len(lines) < expectedChunks {
		t.Fatalf("expected at least %d chunks, got %d", expectedChunks, len(lines))
	}

	// Reassemble and verify no data loss.
	var reassembled strings.Builder
	for _, line := range lines {
		content := stripTokenAndHeader(line, "test-token")
		reassembled.WriteString(content)
	}

	got := reassembled.String()
	if len(got) < msgSize {
		t.Fatalf("reassembled length %d < original %d (data lost in chunking)", len(got), msgSize)
	}

	// Verify content integrity — check that the repeating pattern is intact.
	for i := 0; i < minInt(len(got), msgSize); i++ {
		expected := byte('a' + (i % 26))
		if got[i] != expected {
			t.Fatalf("content mismatch at position %d: expected %c, got %c", i, expected, got[i])
		}
	}
}

// TestAdversarialSmallMessagesRapidFire sends many small messages quickly
// and verifies none are lost (with unlimited concurrency).
func TestAdversarialSmallMessagesRapidFire(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	const count = 1000
	for i := 0; i < count; i++ {
		le.Printf("s%d", i)
	}
	le.Flush()

	waitForLines(t, server, count, 30*time.Second)
	lines := server.Lines()
	if len(lines) != count {
		t.Fatalf("expected exactly %d lines, got %d (messages lost or duplicated)", count, len(lines))
	}
}

// TestAdversarialGoroutineLeakAfterClose verifies no goroutine leak after
// heavy usage followed by Close.
func TestAdversarialGoroutineLeakAfterClose(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	server := newFakeLogentriesServer(t)

	le := connectToFakeServer(t, server, 0, nil)

	// Heavy usage.
	for i := 0; i < 200; i++ {
		le.Printf("leak-test-%d", i)
	}
	le.Flush()
	waitForLines(t, server, 200, 15*time.Second)

	// Close everything.
	le.Close()
	server.Close()

	// Let goroutines wind down.
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - baseline
	if leaked > 5 {
		t.Fatalf("goroutine leak after Close: baseline=%d, after=%d, leaked=%d", baseline, after, leaked)
	}
}

// TestAdversarialWriteAfterCloseSilentDrop verifies that writes after Close
// are silently dropped and don't panic or leak goroutines.
func TestAdversarialWriteAfterCloseSilentDrop(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	// These should all be no-ops (closed flag set).
	for i := 0; i < 100; i++ {
		le.Printf("after-close-%d", i)
	}
	le.Flush()

	time.Sleep(100 * time.Millisecond)
	lines := server.Lines()
	// No messages should have been sent after close.
	for _, line := range lines {
		if strings.Contains(line, "after-close") {
			t.Fatalf("message sent after Close(): %q", line)
		}
	}
}

// TestAdversarialSemaphoreRecoveryAfterBurst tests that after a burst that
// saturates the semaphore, subsequent single writes still succeed.
func TestAdversarialSemaphoreRecoveryAfterBurst(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	const semSize = 3
	le := connectToFakeServer(t, server, semSize, nil)
	defer le.Close()

	// Saturating burst.
	server.SetReadDelay(20 * time.Millisecond)
	for i := 0; i < 50; i++ {
		le.Printf("burst-%d", i)
	}
	le.Flush()

	// Remove delay.
	server.SetReadDelay(0)

	// Now send individual messages — each should succeed because tokens are back.
	for i := 0; i < semSize; i++ {
		le.Printf("post-burst-%d", i)
	}
	le.Flush()

	// Wait for all messages to be read by the server (burst + post-burst).
	waitForLines(t, server, semSize+3, 5*time.Second) // at least burst lines + post-burst

	// Verify post-burst messages arrived.
	found := 0
	for _, line := range server.Lines() {
		if strings.Contains(line, "post-burst") {
			found++
		}
	}
	if found != semSize {
		t.Fatalf("expected %d post-burst messages, got %d (semaphore not fully recovered)", semSize, found)
	}
}

// TestAdversarialWriteRacingClose tests writes racing with Close.
// This should not panic or deadlock.
func TestAdversarialWriteRacingClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	var wg sync.WaitGroup

	// Spawn writers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			le.Printf("racing-%d", n)
		}(i)
	}

	// Close while writes are in flight.
	go func() {
		time.Sleep(1 * time.Millisecond)
		le.Close()
	}()

	wg.Wait()
	// If we get here without deadlock or panic, the test passes.
}

// TestAdversarialFlushOnEmptyLogger verifies Flush on a logger that has
// never had any writes doesn't block or panic.
func TestAdversarialFlushOnEmptyLogger(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Should return immediately.
	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Flush() blocked on empty logger")
	}
}

// TestAdversarialMixedWriteAndPrint tests interleaving Write() (io.Writer)
// and Print() calls concurrently.
func TestAdversarialMixedWriteAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	const count = 100

	// Half via Print (async goroutine internally).
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			le.Printf("print-%d", n)
		}(i)
	}

	// Half via Write (synchronous, holds connMu).
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			le.Write([]byte(fmt.Sprintf("write-%d\n", n)))
		}(i)
	}

	wg.Wait()
	le.Flush()

	// Both Print and Write go through connMu, so they serialize.
	// All 200 messages should arrive.
	waitForLines(t, server, 2*count, 30*time.Second)
	lines := server.Lines()
	if len(lines) < 2*count {
		t.Fatalf("expected %d lines, got %d (messages lost in mixed Write/Print)", 2*count, len(lines))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
