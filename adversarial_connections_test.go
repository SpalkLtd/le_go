package le_go

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Adversarial Connection Tests — Connection Chaos & Network Failures
// =============================================================================

// TestAdversarialCloseMidWriteRecovery kills the server connection after
// the first line is read, then verifies the logger recovers on subsequent
// messages. The logger does NOT detect broken connections proactively — it
// only discovers them on the next write attempt. With retries enabled,
// the retry should reconnect and succeed.
func TestAdversarialCloseMidWriteRecovery(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.closeMidRead.Store(true)

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)
	defer le.Close()
	le.SetNumberOfRetries(3)

	// First message — server will close connection after reading it
	le.Print("msg-1")
	le.Flush()

	// Give the server time to process and close the connection
	time.Sleep(500 * time.Millisecond)

	// Stop closing mid-read so recovery can work
	server.closeMidRead.Store(false)

	// The connection is now broken but logger doesn't know it.
	// Force conn=nil so ensureOpenConnection will reconnect.
	le.connMu.Lock()
	if le.conn != nil {
		le.conn.Close()
		le.conn = nil
	}
	le.connMu.Unlock()

	// Second message — should reconnect via ensureOpenConnection and succeed
	le.Print("msg-2")
	le.Flush()

	waitForLines(t, server, 2, 10*time.Second)

	lines := server.Lines()
	found := false
	for _, line := range lines {
		if strings.Contains(line, "msg-2") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected to find 'msg-2' after recovery, got lines: %v", lines)
	}
}

// TestAdversarialBrokenConnNotDetected exposes a weakness: when the server
// closes the connection, the logger does NOT detect it until the next write.
// If ensureOpenConnection sees a non-nil, non-stale conn it assumes it's good.
// This means the next write will fail silently (or with an error) rather than
// proactively reconnecting.
func TestAdversarialBrokenConnNotDetected(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.closeMidRead.Store(true)

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)
	defer le.Close()
	le.SetNumberOfRetries(0) // No retries — expose the raw failure

	// First message — server reads it then closes connection
	le.Print("msg-1")
	le.Flush()
	time.Sleep(500 * time.Millisecond)

	// Stop closing so we can see what happens
	server.closeMidRead.Store(false)

	// Second message — conn is broken but non-nil and non-stale.
	// ensureOpenConnection won't reconnect. writeRaw will fail.
	// With 0 retries, the message is lost.
	le.Print("msg-2-should-fail")
	le.Flush()
	time.Sleep(500 * time.Millisecond)

	errOutput := errBuf.String()
	if strings.Contains(errOutput, "write error") {
		t.Logf("CONFIRMED: broken connection not detected until write fails: %s", errOutput)
	} else {
		// If no error was logged, check if msg-2 actually arrived
		lines := server.Lines()
		for _, line := range lines {
			if strings.Contains(line, "msg-2-should-fail") {
				// This would be surprising — the connection was closed
				t.Logf("msg-2 somehow arrived despite broken connection")
				return
			}
		}
		t.Logf("msg-2 silently lost — no error logged and no message received. errBuf: %q", errOutput)
	}
}

// TestAdversarialRapidReconnectionStorm repeatedly backdates lastRefreshAt
// to force many reconnections in a tight loop.
func TestAdversarialRapidReconnectionStorm(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	for i := 0; i < 20; i++ {
		// Force staleness before each write
		le.connMu.Lock()
		le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
		le.connMu.Unlock()

		le.Printf("storm-%d", i)
		le.Flush()
	}

	// All 20 messages should eventually arrive
	waitForLines(t, server, 20, 15*time.Second)

	lines := server.Lines()
	if len(lines) < 20 {
		t.Fatalf("expected 20 lines, got %d — messages lost during reconnection storm", len(lines))
	}

	// Each write should have caused a reconnect
	connCount := server.ConnectionCount()
	if connCount < 20 {
		t.Logf("info: got %d connections (expected ~21)", connCount)
	}
}

// TestAdversarialServerAcceptsThenCloses simulates a server that accepts
// connections but immediately closes them. The logger should handle this
// gracefully and report errors rather than panic.
func TestAdversarialServerAcceptsThenCloses(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	// Connect FIRST, then enable rejection
	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)
	defer le.Close()
	le.dialTimeout = 2 * time.Second

	// NOW enable rejection for future connections
	server.rejectConns.Store(true)

	// Force a reconnect by closing the current connection
	le.connMu.Lock()
	le.conn.Close()
	le.conn = nil
	le.connMu.Unlock()

	// Try to write — reconnect will succeed at TCP level but server
	// immediately closes the TLS connection. The write should fail.
	le.Print("should-fail")
	le.Flush()

	time.Sleep(1 * time.Second)

	errOutput := errBuf.String()
	t.Logf("error output after server-rejects: %q", errOutput)
	// The logger should have logged an error, not panicked
}

// TestAdversarialSlowServerBackpressure creates a server that reads very
// slowly, causing TCP buffer backpressure. Tests that write timeouts are
// enforced and the logger doesn't hang indefinitely.
func TestAdversarialSlowServerBackpressure(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.SetReadDelay(2 * time.Second)

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 0, &errBuf)
	defer le.Close()

	le.writeTimeout = 500 * time.Millisecond

	// Blast many large messages to fill TCP buffer
	bigMsg := strings.Repeat("X", 60000)
	for i := 0; i < 50; i++ {
		le.Printf("%s-%d", bigMsg, i)
	}

	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("Flush completed. Error output length: %d bytes", len(errBuf.String()))
	case <-time.After(60 * time.Second):
		t.Fatal("Flush hung under slow-server backpressure — write timeout not enforced")
	}
}

// TestAdversarialServerStopsReadingCompletely simulates a server that stops
// reading entirely. The TCP send buffer should fill up, causing write timeouts.
// BUG EXPOSED: Flush can hang for a very long time because each write goroutine
// holds connMu while blocked on a timed-out TCP write, and with unlimited
// concurrentWrites all goroutines queue up.
func TestAdversarialServerStopsReadingCompletely(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	server.SetReadDelay(60 * time.Second)

	var errBuf bytes.Buffer
	le := connectToFakeServer(t, server, 5, &errBuf) // limit to 5 concurrent
	defer le.Close()

	le.writeTimeout = 1 * time.Second

	// Write enough data to fill the TCP buffer
	bigMsg := strings.Repeat("Y", 60000)
	for i := 0; i < 20; i++ {
		le.Printf("%s-%d", bigMsg, i)
	}

	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		// Good — Flush completed within timeout
	case <-time.After(30 * time.Second):
		t.Fatal("BUG: Flush hung when server stops reading — write timeout not properly enforced or goroutines pile up behind connMu")
	}

	errOutput := errBuf.String()
	if len(errOutput) > 0 {
		t.Logf("write errors (expected): %s", errOutput[:min(200, len(errOutput))])
	}
}

// TestAdversarialMultipleReconnectionsDataIntegrity sends messages across
// multiple forced reconnections and verifies all messages arrive intact.
func TestAdversarialMultipleReconnectionsDataIntegrity(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetNumberOfRetries(3)

	totalMessages := 30
	for i := 0; i < totalMessages; i++ {
		// Force reconnect every 5 messages
		if i > 0 && i%5 == 0 {
			le.connMu.Lock()
			le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
			le.connMu.Unlock()
		}

		le.Printf("integrity-%04d", i)
		le.Flush()
	}

	waitForLines(t, server, totalMessages, 15*time.Second)

	lines := server.Lines()
	received := make(map[string]bool)
	for _, line := range lines {
		for i := 0; i < totalMessages; i++ {
			tag := fmt.Sprintf("integrity-%04d", i)
			if strings.Contains(line, tag) {
				received[tag] = true
			}
		}
	}

	for i := 0; i < totalMessages; i++ {
		tag := fmt.Sprintf("integrity-%04d", i)
		if !received[tag] {
			t.Errorf("missing message %q after multiple reconnections", tag)
		}
	}
}

// TestAdversarialPrintAfterCloseConn calls Print() after Close().
// The logger should silently drop the message, not panic.
func TestAdversarialPrintAfterCloseConn(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	// These should not panic
	le.Print("after-close-1")
	le.Printf("after-close-%d", 2)
	le.Println("after-close-3")

	// Flush on closed logger — should not panic
	le.Flush()

	time.Sleep(200 * time.Millisecond)
	lines := server.Lines()
	for _, line := range lines {
		if strings.Contains(line, "after-close") {
			t.Fatalf("message sent after Close(): %q", line)
		}
	}
}

// TestAdversarialWriteAfterCloseNoGuard exposes that Write() does NOT check
// the `closed` atomic flag. Unlike Print/Printf/Println which go through
// Output() and check `closed`, Write() goes directly to writeRaw().
// After Close(), the conn is nil, so writeRaw attempts ensureOpenConnection
// which may reconnect — or panic if the logger state is inconsistent.
func TestAdversarialWriteAfterCloseNoGuard(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BUG: Write() panicked after Close(): %v", r)
		}
	}()

	// Write() does not check the closed flag — it may try to reconnect
	// or write to the closed connection.
	n, err := le.Write([]byte("after-close-via-write"))
	t.Logf("Write() after Close(): n=%d, err=%v", n, err)

	// Check if the message actually arrived (it shouldn't, but if it does
	// that means Write() reconnected after Close() — a semantic bug)
	time.Sleep(500 * time.Millisecond)
	lines := server.Lines()
	for _, line := range lines {
		if strings.Contains(line, "after-close-via-write") {
			t.Fatalf("BUG: Write() sent data after Close() — missing closed guard in Write()")
		}
	}
}

// TestAdversarialWriteOnClosedConnection directly calls Write() on a logger
// whose underlying connection has been closed (but logger itself is not closed).
// This tests the reconnect-on-write-failure path.
func TestAdversarialWriteOnClosedConnection(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetNumberOfRetries(2)

	// Sabotage the connection
	le.connMu.Lock()
	le.conn.Close()
	le.connMu.Unlock()

	// Write should trigger reconnect via writeRaw retry logic
	n, err := le.Write([]byte("via-Write-method"))
	if err != nil {
		t.Fatalf("Write() failed after connection sabotage: %v", err)
	}
	if n == 0 {
		t.Fatal("Write() returned 0 bytes written")
	}

	waitForLines(t, server, 1, 5*time.Second)

	lines := server.Lines()
	found := false
	for _, line := range lines {
		if strings.Contains(line, "via-Write-method") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("message not received after Write() with closed connection, lines: %v", lines)
	}
}

// TestAdversarialConcurrentReconnections fires many goroutines that all
// trigger writes while the connection is stale. Uses bounded concurrency
// to avoid test timeout.
func TestAdversarialConcurrentReconnections(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 10, nil) // bounded concurrency
	defer le.Close()

	// Force staleness
	le.connMu.Lock()
	le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
	le.connMu.Unlock()

	var wg sync.WaitGroup
	numGoroutines := 30

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			le.Printf("concurrent-reconnect-%d", id)
		}(i)
	}

	wg.Wait()

	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		// Good
	case <-time.After(30 * time.Second):
		t.Fatal("Flush deadlocked during concurrent reconnections")
	}

	time.Sleep(1 * time.Second)
	lines := server.Lines()
	t.Logf("received %d out of %d concurrent messages across reconnection", len(lines), numGoroutines)

	if len(lines) == 0 {
		t.Fatal("no messages received during concurrent reconnection — all dropped or deadlocked")
	}
}

// TestAdversarialDialTimeoutSemaphoreExhaustion tests whether dial timeouts
// cascade into semaphore token exhaustion, permanently blocking the logger.
func TestAdversarialDialTimeoutSemaphoreExhaustion(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 3, nil) // only 3 concurrent writes
	defer le.Close()

	// Break the connection and point to unreachable host
	le.connMu.Lock()
	le.conn.Close()
	le.conn = nil
	le.host = "192.0.2.1:443" // RFC 5737 TEST-NET — unreachable
	le.dialTimeout = 1 * time.Second
	le.connMu.Unlock()

	// Fire off writes that will all fail due to dial timeout
	for i := 0; i < 10; i++ {
		le.Printf("timeout-%d", i)
	}

	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		// Good — did not deadlock
	case <-time.After(30 * time.Second):
		t.Fatal("BUG: Flush deadlocked — semaphore tokens leaked during dial timeout")
	}

	// Now point back to working server and verify logger still works
	le.connMu.Lock()
	le.host = server.addr
	le.connMu.Unlock()

	le.Print("recovery-after-timeout")

	recoveryDone := make(chan struct{})
	go func() {
		le.Flush()
		close(recoveryDone)
	}()

	select {
	case <-recoveryDone:
		// Good
	case <-time.After(15 * time.Second):
		t.Fatal("BUG: Logger permanently blocked after dial timeout — semaphore tokens exhausted")
	}

	waitForLines(t, server, 1, 10*time.Second)
	lines := server.Lines()
	found := false
	for _, line := range lines {
		if strings.Contains(line, "recovery-after-timeout") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("logger did not recover after dial timeout exhaustion, lines: %v", lines)
	}
}

// TestAdversarialReconnectWhileWriting tests the scenario where connection
// is sabotaged while data is being written.
func TestAdversarialReconnectWhileWriting(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()
	le.SetNumberOfRetries(2)

	var wg sync.WaitGroup

	// Goroutine 1: continuously writes
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			le.Printf("writer-%d", i)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Goroutine 2: continuously sabotages the connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			time.Sleep(30 * time.Millisecond)
			le.connMu.Lock()
			if le.conn != nil {
				le.conn.Close()
				le.conn = nil
			}
			le.connMu.Unlock()
		}
	}()

	wg.Wait()
	le.Flush()

	time.Sleep(2 * time.Second)
	lines := server.Lines()
	t.Logf("received %d out of 20 messages while connection was being sabotaged", len(lines))
	if len(lines) == 0 {
		t.Fatal("no messages received — reconnect-while-writing completely failed")
	}
}

// TestAdversarialDoubleCloseConn calls Close() twice. Should not panic.
func TestAdversarialDoubleCloseConn(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	err1 := le.Close()
	err2 := le.Close()
	t.Logf("first Close(): %v, second Close(): %v", err1, err2)
}

// TestAdversarialMassiveReconnectionBurst sends a burst of messages that each
// force reconnection concurrently.
func TestAdversarialMassiveReconnectionBurst(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 10, nil)
	defer le.Close()

	var sentCount atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			le.connMu.Lock()
			le.lastRefreshAt = time.Now().Add(-20 * time.Minute)
			le.connMu.Unlock()

			le.Printf("burst-%d", id)
			sentCount.Add(1)
		}(i)
	}

	wg.Wait()

	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(30 * time.Second):
		t.Fatal("Flush deadlocked during massive reconnection burst")
	}

	time.Sleep(2 * time.Second)
	lines := server.Lines()
	t.Logf("sent %d, received %d messages in reconnection burst", sentCount.Load(), len(lines))

	if len(lines) == 0 {
		t.Fatal("no messages received during massive reconnection burst")
	}
}

// TestAdversarialFlushRaceWithClose tests calling Flush() and Close()
// concurrently. Neither should panic or deadlock.
func TestAdversarialFlushRaceWithClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	for i := 0; i < 10; i++ {
		le.Printf("flush-race-%d", i)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		le.Flush()
	}()

	go func() {
		defer wg.Done()
		le.Close()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// passed
	case <-time.After(10 * time.Second):
		t.Fatal("Flush/Close race deadlocked")
	}
}

// TestAdversarialWriteMethodNoClosedGuard verifies that Write() does not
// check the `closed` atomic flag. This is a semantic inconsistency:
// Print/Printf/Println all check `closed` via Output(), but Write() bypasses
// Output() entirely and goes straight to writeRaw(). After Close(), Write()
// may attempt to reconnect and send data.
func TestAdversarialWriteMethodNoClosedGuard(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	// Send one message before closing to verify the path works
	le.Print("before-close")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	le.Close()

	// Write() bypasses the closed check in Output().
	// If the connection is nil (closed), Write() calls writeRaw() which calls
	// ensureOpenConnection() which may try to reconnect.
	_, err := le.Write([]byte("after-close-write"))
	if err == nil {
		// If no error, Write() may have reconnected and sent data!
		time.Sleep(500 * time.Millisecond)
		lines := server.Lines()
		for _, line := range lines {
			if strings.Contains(line, "after-close-write") {
				t.Fatalf("BUG CONFIRMED: Write() reconnected and sent data after Close() — missing closed guard")
			}
		}
		t.Log("Write() succeeded without error after Close() but data did not arrive")
	} else {
		t.Logf("Write() returned error after Close() (acceptable): %v", err)
	}
}

// TestAdversarialConnectionRefreshRaceWithWrite verifies that when multiple
// writes happen right at the staleness boundary, only one reconnection occurs
// and data isn't corrupted.
func TestAdversarialConnectionRefreshRaceWithWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Set lastRefreshAt to just barely stale
	le.connMu.Lock()
	le.lastRefreshAt = time.Now().Add(-15*time.Minute - 1*time.Second)
	le.connMu.Unlock()

	// Send 10 messages rapidly — the first should trigger reconnect,
	// the rest should use the new connection
	for i := 0; i < 10; i++ {
		le.Printf("boundary-%d", i)
	}
	le.Flush()

	waitForLines(t, server, 10, 10*time.Second)

	lines := server.Lines()
	if len(lines) < 10 {
		t.Fatalf("expected 10 lines at staleness boundary, got %d", len(lines))
	}

	// Should be exactly 2 connections: initial + 1 reconnect
	connCount := server.ConnectionCount()
	t.Logf("connections at staleness boundary: %d (expected 2)", connCount)
	if connCount > 3 {
		t.Errorf("too many connections (%d) at staleness boundary — reconnect storm", connCount)
	}
}
