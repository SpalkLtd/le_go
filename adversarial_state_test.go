package le_go

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Adversarial State Corruption & Dangerous Interleaving Tests
// =============================================================================

// TestAdversarialSetFlagsDuringWrite hammers SetFlags while writes are in
// flight. formatHeader reads l.flag and l.prefix WITHOUT holding l.mu,
// so concurrent SetFlags can cause formatHeader to see a torn/inconsistent
// flag value. The race detector should catch this.
func TestAdversarialSetFlagsDuringWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	const iterations = 200

	// Writer goroutine — keeps printing
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			le.Printf("msg-%d", i)
		}
	}()

	// Flag mutator goroutine — rapidly changes flags
	wg.Add(1)
	go func() {
		defer wg.Done()
		flags := []int{0, log.Ldate, log.Ltime, log.Ldate | log.Ltime | log.Lmicroseconds, log.Lshortfile, log.LUTC | log.Ldate}
		for i := 0; i < iterations; i++ {
			le.SetFlags(flags[i%len(flags)])
		}
	}()

	wg.Wait()
	le.Flush()
}

// TestAdversarialSetPrefixDuringWrite hammers SetPrefix while writes are
// in flight. formatHeader reads l.prefix without holding l.mu (it's called
// from writeMessage which only holds connMu). Race detector should fire.
func TestAdversarialSetPrefixDuringWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	const iterations = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			le.Printf("msg-%d", i)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		prefixes := []string{"", "SHORT", "A-MUCH-LONGER-PREFIX-THAT-MIGHT-CAUSE-ISSUES "}
		for i := 0; i < iterations; i++ {
			le.SetPrefix(prefixes[i%len(prefixes)])
		}
	}()

	wg.Wait()
	le.Flush()
}

// TestAdversarialSetErrorOutputDuringWrite changes the error output writer
// while writeToErrOutput might be reading it. This tests the errOutputMutex.
func TestAdversarialSetErrorOutputDuringWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	var buf1, buf2 bytes.Buffer
	le := connectToFakeServer(t, server, 0, &buf1)

	// Close the underlying conn to force write errors (which trigger writeToErrOutput)
	le.conn.Close()
	le.conn = nil
	le.host = "127.0.0.1:1"
	le.dialTimeout = 50 * time.Millisecond

	var wg sync.WaitGroup
	const iterations = 50

	// Writer goroutine — each Print will fail and call writeToErrOutput
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			le.Printf("failing-msg-%d", i)
		}
	}()

	// Mutator goroutine — swap error output back and forth
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				le.SetErrorOutput(&buf2)
			} else {
				le.SetErrorOutput(&buf1)
			}
		}
	}()

	wg.Wait()
	le.Flush()
	// No crash = test passes for now, race detector is the real judge
}

// TestAdversarialCloseWhileWriteInProgress calls Close() while Print()
// goroutines are actively writing. This tests the interaction between
// atomic.StoreInt32(&closed,1) + wg.Wait() in Close, and wg.Add(1) in Output.
//
// KEY RACE: Output checks `closed`, then does wg.Add(1). Between those two
// operations, Close can set closed=1 and call wg.Wait(). The wg.Add(1)
// happens after wg.Wait() starts, which is a WaitGroup misuse that panics.
func TestAdversarialCloseWhileWriteInProgress(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	// Slow down the server to keep writes in flight longer
	server.SetReadDelay(10 * time.Millisecond)

	var wg sync.WaitGroup
	const writers = 10
	const msgsPerWriter = 50

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < msgsPerWriter; j++ {
				le.Printf("writer-%d-msg-%d", id, j)
			}
		}(i)
	}

	// Give writers a head start, then close
	time.Sleep(5 * time.Millisecond)
	le.Close()

	wg.Wait()
}

// TestAdversarialCloseBetweeenCheckAndAdd tries to exploit the exact window
// between the closed check and wg.Add(1) in Output. We use many goroutines
// to maximize the chance of hitting this window.
func TestAdversarialCloseBetweenCheckAndAdd(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 0, nil)

		var startGate sync.WaitGroup
		startGate.Add(1)

		const numWriters = 50
		var writersDone sync.WaitGroup
		writersDone.Add(numWriters)

		for i := 0; i < numWriters; i++ {
			go func(id int) {
				defer writersDone.Done()
				startGate.Wait() // all start at the same time
				le.Printf("writer-%d", id)
			}(i)
		}

		// Release all writers and immediately close — maximum contention
		startGate.Done()
		le.Close()

		writersDone.Wait()
		server.Close()
	}
}

// TestAdversarialFlushThenPrint verifies that Flush followed immediately by
// Print doesn't cause wg.Add after wg.Wait has started returning.
func TestAdversarialFlushThenPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	for i := 0; i < 100; i++ {
		le.Printf("before-flush-%d", i)
	}

	// Flush and immediately print — tests wg.Add/wg.Wait interleaving
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		le.Flush()
	}()
	go func() {
		defer wg.Done()
		// Tight loop of prints while Flush is in progress
		for j := 0; j < 100; j++ {
			le.Printf("during-flush-%d", j)
		}
	}()
	wg.Wait()
	le.Flush()
}

// TestAdversarialDoubleCloseState calls Close() twice. The second call should
// not panic or return an error from closing an already-closed connection.
func TestAdversarialDoubleCloseState(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	err1 := le.Close()
	if err1 != nil {
		t.Fatalf("first Close returned error: %v", err1)
	}

	// Second close — should not panic
	err2 := le.Close()
	// We accept either nil or an error, just no panic
	_ = err2
}

// TestAdversarialTripleCloseState pushes it further.
func TestAdversarialTripleCloseState(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Close()
	le.Close()
	le.Close()
	// No panic = pass
}

// TestAdversarialCloseThenFlush calls Close then Flush. Flush's recover
// should protect against WaitGroup misuse.
func TestAdversarialCloseThenFlush(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("msg before close")
	le.Close()

	// Flush after close — should not panic (Flush has recover)
	le.Flush()
}

// TestAdversarialFlushClosePrint does Flush, Close, then Print to verify
// clean shutdown behavior.
func TestAdversarialFlushClosePrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("before flush")
	le.Flush()
	le.Close()

	// Print after close — should silently drop (closed check)
	le.Print("after close")
	// Should not panic or write
}

// TestAdversarialSetNumberOfRetriesRace sets numRetries while a retry loop
// is potentially executing. SetNumberOfRetries has NO synchronization at all,
// so the race detector should flag this.
func TestAdversarialSetNumberOfRetriesDuringWrite(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	le.SetNumberOfRetries(3)

	// Force writes to fail so we enter the retry loop
	server.SetReadDelay(10 * time.Millisecond)

	var wg sync.WaitGroup
	const iterations = 100

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			le.Printf("msg-%d", i)
		}
	}()

	// Change numRetries while writes (and potential retries) are in progress
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			le.SetNumberOfRetries(i % 5)
		}
	}()

	wg.Wait()
	le.Flush()
}

// TestAdversarialInterleaveWriteAndPrint verifies that token appears correctly
// when Print (which goes through Output/writeMessage) and Write (direct) are
// interleaved. Both should produce lines starting with "test-token ".
func TestAdversarialInterleaveWriteAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	const count = 50

	// Print goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			le.Printf("print-msg-%d", i)
		}
	}()

	// Write goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			le.Write([]byte(fmt.Sprintf("write-msg-%d", i)))
		}
	}()

	wg.Wait()
	le.Flush()

	waitForLines(t, server, count*2, 10*time.Second)

	lines := server.Lines()
	for i, line := range lines {
		if !strings.HasPrefix(line, "test-token ") {
			t.Errorf("line %d missing token prefix: %q", i, line)
		}
	}
}

// TestAdversarialFlagChangesBetweenPrints verifies that changing flags between
// prints produces the correct header format for each line.
func TestAdversarialFlagChangesBetweenPrints(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Print with no flags
	le.SetFlags(0)
	le.Print("no-flags")
	le.Flush()

	// Print with date flag
	le.SetFlags(log.Ldate)
	le.Print("with-date")
	le.Flush()

	// Print with no flags again
	le.SetFlags(0)
	le.Print("no-flags-again")
	le.Flush()

	waitForLines(t, server, 3, 5*time.Second)

	lines := server.Lines()
	if len(lines) < 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	// First and third lines should have no date, second should have a date
	msg0 := stripTokenAndHeader(lines[0], "test-token")
	msg1 := stripTokenAndHeader(lines[1], "test-token")
	msg2 := stripTokenAndHeader(lines[2], "test-token")

	if msg0 != "no-flags" {
		t.Errorf("line 0: expected 'no-flags', got %q", msg0)
	}
	// msg1 should have a date prefix before "with-date"
	if !strings.Contains(msg1, "with-date") {
		t.Errorf("line 1: expected to contain 'with-date', got %q", msg1)
	}
	if !strings.Contains(lines[1], "/") {
		t.Errorf("line 1: expected date separator '/', got %q", lines[1])
	}
	if msg2 != "no-flags-again" {
		t.Errorf("line 2: expected 'no-flags-again', got %q", msg2)
	}
}

// TestAdversarialPrintFromInsidePrintCallback explores what happens when
// Output's doAsync callback itself calls Print. This creates nested wg
// interactions and potentially recursive locking issues.
func TestAdversarialPrintFromInsidePrintCallback(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Use Output directly with a callback that prints again
	le.Output(2, "outer-message", func() {
		le.Print("inner-message-from-callback")
	})

	le.Flush()
	waitForLines(t, server, 2, 5*time.Second)

	lines := server.Lines()
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}

	foundOuter := false
	foundInner := false
	for _, line := range lines {
		if strings.Contains(line, "outer-message") {
			foundOuter = true
		}
		if strings.Contains(line, "inner-message-from-callback") {
			foundInner = true
		}
	}
	if !foundOuter {
		t.Error("missing outer-message")
	}
	if !foundInner {
		t.Error("missing inner-message-from-callback")
	}
}

// TestAdversarialMessageOrdering sends numbered messages and verifies
// the server receives them all (ordering may vary due to goroutine scheduling,
// but all messages should arrive).
func TestAdversarialMessageOrdering(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	const count = 100
	for i := 0; i < count; i++ {
		le.Printf("seq-%04d", i)
	}
	le.Flush()

	waitForLines(t, server, count, 10*time.Second)

	lines := server.Lines()
	seen := make(map[string]bool)
	for _, line := range lines {
		for i := 0; i < count; i++ {
			tag := fmt.Sprintf("seq-%04d", i)
			if strings.Contains(line, tag) {
				seen[tag] = true
			}
		}
	}

	for i := 0; i < count; i++ {
		tag := fmt.Sprintf("seq-%04d", i)
		if !seen[tag] {
			t.Errorf("missing message: %s", tag)
		}
	}
}

// TestAdversarialConcurrentFlushAndClose races Flush and Close against
// each other from different goroutines.
func TestAdversarialConcurrentFlushAndClose(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 0, nil)

		// Send some messages
		for i := 0; i < 20; i++ {
			le.Printf("msg-%d", i)
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
		wg.Wait()
		server.Close()
	}
}

// TestAdversarialWriteAfterCloseState verifies that Write() after Close()
// doesn't panic. Note: Write() does NOT check the closed flag — it goes
// straight to connMu + writeRaw. This may cause a write to a closed conn.
func TestAdversarialWriteAfterCloseState(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Close()

	// Write after close — should not panic even though conn is closed
	_, err := le.Write([]byte("after-close-message"))
	// We expect an error (closed conn) but no panic
	_ = err
}

// TestAdversarialMassiveConcurrentSetFlagsAndPrint is a stress test that
// maximizes contention on the flag/prefix fields while formatHeader is reading.
func TestAdversarialMassiveConcurrentSetFlagsAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 5 writers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
					le.Printf("w%d-m%d", id, j)
				}
			}
		}(i)
	}

	// 3 flag mutators
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			flags := []int{0, log.Ldate, log.Ltime, log.Lmicroseconds, log.Lshortfile, log.Llongfile}
			j := 0
			for {
				select {
				case <-stop:
					return
				default:
					le.SetFlags(flags[j%len(flags)])
					j++
				}
			}
		}()
	}

	// 2 prefix mutators
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prefixes := []string{"", "A", "LONGPREFIX: ", "X: "}
			j := 0
			for {
				select {
				case <-stop:
					return
				default:
					le.SetPrefix(prefixes[j%len(prefixes)])
					j++
				}
			}
		}()
	}

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	le.Flush()
}

// TestAdversarialConcurrentCloseFromMultipleGoroutines calls Close()
// simultaneously from many goroutines.
func TestAdversarialConcurrentCloseFromMultipleGoroutines(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("before close")

	var startGate sync.WaitGroup
	startGate.Add(1)

	const closers = 10
	var done sync.WaitGroup
	done.Add(closers)

	for i := 0; i < closers; i++ {
		go func() {
			defer done.Done()
			startGate.Wait()
			le.Close()
		}()
	}

	startGate.Done() // release all closers simultaneously
	done.Wait()
}

// TestAdversarialPrintAfterCloseIssilent verifies that Print after Close
// silently drops messages (due to atomic closed check in Output).
func TestAdversarialPrintAfterCloseIsSilent(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.Print("before")
	le.Flush()
	waitForLines(t, server, 1, 5*time.Second)

	le.Close()

	// These should all be silently dropped
	for i := 0; i < 100; i++ {
		le.Printf("after-close-%d", i)
	}

	// Give time for any stray messages to arrive
	time.Sleep(200 * time.Millisecond)

	lines := server.Lines()
	for _, line := range lines {
		if strings.Contains(line, "after-close") {
			t.Errorf("message leaked after Close: %q", line)
		}
	}
}

// TestAdversarialSetNumberOfRetriesStateCorruption directly demonstrates
// the data race in SetNumberOfRetries. It's read from writeRaw (under connMu)
// but written from SetNumberOfRetries (no lock at all).
func TestAdversarialSetNumberOfRetriesStateCorruption(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var done int32

	// Writer — every write reads numRetries from writeRaw
	go func() {
		for atomic.LoadInt32(&done) == 0 {
			le.Print("retry-race-test")
		}
	}()

	// Mutator — no lock
	go func() {
		for atomic.LoadInt32(&done) == 0 {
			le.SetNumberOfRetries(0)
			le.SetNumberOfRetries(1)
			le.SetNumberOfRetries(2)
			le.SetNumberOfRetries(3)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	atomic.StoreInt32(&done, 1)
	le.Flush()
}

// TestAdversarialBufSharedAcrossChunks verifies that the shared l.buf
// field doesn't cause corruption when multiple writeMessage calls interleave.
// Both writeMessage and Write use connMu, but buf is a struct field reused.
func TestAdversarialBufSharedAcrossChunks(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	const count = 50

	// Multiple Print goroutines — each goes through writeMessage which uses l.buf
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < count; j++ {
				le.Printf("goroutine-%d-msg-%d", id, j)
			}
		}(i)
	}

	// Also interleave Write calls which use their own local buf
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < count; j++ {
				le.Write([]byte(fmt.Sprintf("direct-write-%d-%d", id, j)))
			}
		}(i)
	}

	wg.Wait()
	le.Flush()

	waitForLines(t, server, count*10, 15*time.Second)

	// Verify every line starts with the token (no buf corruption)
	lines := server.Lines()
	for i, line := range lines {
		if !strings.HasPrefix(line, "test-token ") {
			t.Errorf("line %d corrupted token prefix: %q", i, line[:min(50, len(line))])
		}
	}
}

