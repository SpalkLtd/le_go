package le_go

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Adversarial Concurrency Tests
// =============================================================================

// TestAdversarialConcurrentPrintAndClose hammers the race window between
// Output's wg.Add(1) and Close's wg.Wait(). If Close's atomic store of
// `closed` and subsequent wg.Wait() interleaves with Output checking closed
// then calling wg.Add(1), we get "sync: WaitGroup is reused before previous
// Wait has returned" or a panic.
func TestAdversarialConcurrentPrintAndClose(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 0, nil)

		// Spawn many writers that keep printing
		var writerWg sync.WaitGroup
		stop := make(chan struct{})
		for i := 0; i < 20; i++ {
			writerWg.Add(1)
			go func(id int) {
				defer writerWg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						le.Print(fmt.Sprintf("writer-%d", id))
					}
				}
			}(i)
		}

		// Let writers run briefly, then close (this is the adversarial part)
		runtime.Gosched()
		time.Sleep(1 * time.Millisecond)

		// Close should not panic with "negative WaitGroup counter"
		// or "WaitGroup is reused before previous Wait has returned"
		le.Close()
		close(stop)
		writerWg.Wait()
		server.Close()
	}
}

// TestAdversarialConcurrentFlushAndPrint tests that Flush and Print
// can be called concurrently without WaitGroup panics. The danger:
// Flush calls wg.Wait() while Print calls wg.Add(1) — if a Print's
// wg.Add(1) happens after Wait starts but before it returns, panic.
func TestAdversarialConcurrentFlushAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Continuous flusher
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				le.Flush()
			}
		}
	}()

	// Continuous printer
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				select {
				case <-stop:
					return
				default:
					le.Printf("flush-print-%d-%d", id, j)
				}
			}
		}(i)
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestAdversarialSetNumberOfRetriesRace exposes a data race:
// SetNumberOfRetries writes numRetries without any lock, while writeRaw
// reads it under connMu. Two goroutines calling SetNumberOfRetries
// concurrently is a straight data race.
func TestAdversarialSetNumberOfRetriesRace(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup

	// Writer goroutines
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				le.Printf("retry-race-%d-%d", id, j)
			}
		}(i)
	}

	// Concurrent SetNumberOfRetries — no lock protects this field
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				le.SetNumberOfRetries(j % 5)
			}
		}(i)
	}

	wg.Wait()
	le.Flush()
}

// TestAdversarialConcurrentSetFlagsAndPrint tests for races between
// SetFlags (writes l.flag under mu) and Output (reads l.flag under mu,
// then releases mu and proceeds). The formatHeader function reads
// l.prefix without holding mu — this is a race if SetPrefix is called
// concurrently with writeMessage.
func TestAdversarialConcurrentSetFlagsAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup

	// Toggle flags rapidly between 0 and Lshortfile
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if i%2 == 0 {
				le.SetFlags(log.Lshortfile | log.Ldate | log.Ltime)
			} else {
				le.SetFlags(0)
			}
		}
	}()

	// Toggle prefix rapidly
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			le.SetPrefix(fmt.Sprintf("PFX-%d ", i))
		}
	}()

	// Print concurrently — formatHeader reads prefix/flag, potentially
	// racing with the mutations above if locks aren't held properly
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				le.Printf("flags-race-%d-%d", id, j)
			}
		}(i)
	}

	wg.Wait()
	le.Flush()
}

// TestAdversarialPrefixRaceInFormatHeader exposes the race in formatHeader:
// Output reads l.flag under mu, releases mu, then writeMessage is called
// in a goroutine which calls formatHeader — formatHeader reads l.prefix
// WITHOUT holding mu. If SetPrefix runs concurrently, that's a data race
// on l.prefix (string header, so concurrent read/write = race).
func TestAdversarialPrefixRaceInFormatHeader(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	le.SetFlags(log.Ldate) // ensure formatHeader runs the prefix path
	defer le.Close()

	var wg sync.WaitGroup

	// Continuously mutate prefix
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			le.SetPrefix(strings.Repeat("X", i%50))
		}
	}()

	// Continuously print (triggers formatHeader which reads prefix unsafely)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				le.Print("prefix-race")
			}
		}(i)
	}

	wg.Wait()
	le.Flush()
}

// TestAdversarialConcurrentWriteAndClose tests Write (the io.Writer method)
// racing with Close. Write acquires connMu and writes; Close sets closed
// atomically then acquires connMu. But Write does NOT check the closed flag,
// so it can write to a connection that Close is about to close, or has
// already closed. This can cause errors or panics.
func TestAdversarialConcurrentWriteAndClose(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 0, nil)

		var writerWg sync.WaitGroup
		stop := make(chan struct{})

		for i := 0; i < 10; i++ {
			writerWg.Add(1)
			go func(id int) {
				defer writerWg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						le.Write([]byte(fmt.Sprintf("write-%d\n", id)))
					}
				}
			}(i)
		}

		runtime.Gosched()
		time.Sleep(500 * time.Microsecond)
		le.Close()
		close(stop)
		writerWg.Wait()
		server.Close()
	}
}

// TestAdversarialSemaphoreExhaustion tests that with a small concurrentWrites
// limit, the semaphore doesn't leak tokens. If tokens leak (aren't returned
// on error paths), the logger becomes permanently unable to log.
func TestAdversarialSemaphoreExhaustion(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	// Only allow 2 concurrent writes
	le := connectToFakeServer(t, server, 2, nil)
	defer le.Close()

	// Blast many messages — with only 2 slots, most should be dropped (fast)
	// but the semaphore tokens should be returned correctly
	for i := 0; i < 1000; i++ {
		le.Printf("semaphore-test-%d", i)
	}
	le.Flush()

	// Now verify the semaphore is not exhausted: we should still be able to log
	le.Print("after-exhaust-test")
	le.Flush()

	// Wait for at least one line to arrive (the "after" message or earlier ones)
	waitForLines(t, server, 1, 5*time.Second)
}

// TestAdversarialConcurrentSetErrorOutputAndPrint tests that SetErrorOutput
// and error-path writes (writeToErrOutput) don't race. If a write error
// occurs while SetErrorOutput is changing the writer, we could get a race
// on errOutput.
func TestAdversarialConcurrentSetErrorOutputAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup

	writers := make([]io.Writer, 10)
	for i := range writers {
		writers[i] = &bytes.Buffer{}
	}

	// Rapidly swap error output
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			le.SetErrorOutput(writers[i%len(writers)])
		}
	}()

	// Print messages concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				le.Printf("err-output-race-%d-%d", id, j)
			}
		}(i)
	}

	wg.Wait()
	le.Flush()
}

// TestAdversarialGoroutineLeakOnClose verifies that Close actually waits
// for all in-flight goroutines. If goroutines leak, the goroutine count
// will grow across iterations.
func TestAdversarialGoroutineLeakOnClose(t *testing.T) {
	// Force GC and get baseline goroutine count
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for iter := 0; iter < 20; iter++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 5, nil)

		for i := 0; i < 100; i++ {
			le.Printf("leak-test-%d-%d", iter, i)
		}
		le.Flush()
		le.Close()
		server.Close()
	}

	// Allow goroutines to wind down
	runtime.GC()
	time.Sleep(200 * time.Millisecond)

	final := runtime.NumGoroutine()
	// Allow some headroom for runtime goroutines
	if final > baseline+10 {
		t.Errorf("possible goroutine leak: baseline=%d, final=%d (delta=%d)",
			baseline, final, final-baseline)
	}
}

// TestAdversarialCloseFlushClose tests that calling Close, Flush, Close
// in sequence doesn't panic. The Flush recovery should catch WaitGroup
// misuse but this tests the edge case.
func TestAdversarialCloseFlushClose(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)

	le.Print("before-close")
	le.Close()

	// Flush after Close — wg.Wait() on a WaitGroup that has been
	// used and drained. Should not panic (Flush has recover).
	le.Flush()

	// Second Close — conn is already closed
	err := le.Close()
	_ = err // may or may not error, but should not panic
}

// TestAdversarialMassiveConcurrentPrint tests 100 goroutines each sending
// 100 messages with various API methods to maximize lock contention.
func TestAdversarialMassiveConcurrentPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				switch j % 4 {
				case 0:
					le.Print(fmt.Sprintf("mass-%d-%d", id, j))
				case 1:
					le.Printf("mass-%d-%d", id, j)
				case 2:
					le.Println(fmt.Sprintf("mass-%d-%d", id, j))
				case 3:
					le.Write([]byte(fmt.Sprintf("mass-%d-%d\n", id, j)))
				}
			}
		}(i)
	}

	wg.Wait()
	le.Flush()

	// Verify at least some lines arrived (with no semaphore limit, all should)
	waitForLines(t, server, 100, 10*time.Second)
}

// TestAdversarialConcurrentMultipleClose tests calling Close from many
// goroutines simultaneously. The second atomic store to closed is fine,
// but the second wg.Wait() + conn.Close() might race.
func TestAdversarialConcurrentMultipleClose(t *testing.T) {
	for iter := 0; iter < 30; iter++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 0, nil)

		le.Print("before-multi-close")

		var closeWg sync.WaitGroup
		var closeErrors int64
		for i := 0; i < 10; i++ {
			closeWg.Add(1)
			go func() {
				defer closeWg.Done()
				if err := le.Close(); err != nil {
					atomic.AddInt64(&closeErrors, 1)
				}
			}()
		}
		closeWg.Wait()
		server.Close()
	}
}

// TestAdversarialPrintAfterClose verifies Print after Close is safe.
// Output checks `closed` atomically and returns early, but there's a
// TOCTOU window: the check passes, then Close sets closed and calls
// wg.Wait(), then Output does wg.Add(1) — boom.
func TestAdversarialPrintAfterClose(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		server := newFakeLogentriesServer(t)
		le := connectToFakeServer(t, server, 0, nil)

		// Start a goroutine that keeps printing
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 100; i++ {
				le.Print(fmt.Sprintf("post-close-%d", i))
			}
		}()

		// Close while prints are in flight
		runtime.Gosched()
		le.Close()
		<-done
		server.Close()
	}
}

// TestAdversarialFlushDuringHeavyWrites tests that Flush returns in
// reasonable time even when many writes are in flight. This catches
// deadlocks where connMu and wg interactions block indefinitely.
func TestAdversarialFlushDuringHeavyWrites(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	// Blast messages
	for i := 0; i < 500; i++ {
		le.Printf("heavy-write-%d", i)
	}

	// Flush should not deadlock
	done := make(chan struct{})
	go func() {
		le.Flush()
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(30 * time.Second):
		t.Fatal("Flush deadlocked during heavy writes")
	}
}

// TestAdversarialConcurrentWriteAndPrint tests Write (io.Writer) and Print
// running concurrently. Both acquire connMu, but Print goes through Output
// which spawns a goroutine, while Write is synchronous. This maximizes
// contention on connMu.
func TestAdversarialConcurrentWriteAndPrint(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup

	// Synchronous Write callers
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				le.Write([]byte(fmt.Sprintf("write-%d-%d\n", id, j)))
			}
		}(i)
	}

	// Async Print callers
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				le.Printf("print-%d-%d", id, j)
			}
		}(i)
	}

	wg.Wait()
	le.Flush()

	waitForLines(t, server, 100, 10*time.Second)
}

// TestAdversarialFlagRaceInOutput targets a specific race: Output reads
// l.flag under mu, then RELEASES mu before calling runtime.Caller and
// spawning the goroutine. The goroutine then calls writeMessage which
// calls formatHeader, which reads l.flag again (WITHOUT mu). If SetFlags
// is called between Output's read and formatHeader's read, we get
// inconsistent flag state and potentially a data race.
func TestAdversarialFlagRaceInOutput(t *testing.T) {
	server := newFakeLogentriesServer(t)
	defer server.Close()

	le := connectToFakeServer(t, server, 0, nil)
	defer le.Close()

	var wg sync.WaitGroup

	// Rapidly toggle Lshortfile on and off
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			le.SetFlags(log.Lshortfile)
			le.SetFlags(0)
		}
	}()

	// Print concurrently — the goroutine spawned by Output reads flag
	// in formatHeader without mu, racing with SetFlags
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				le.Print("flag-race-output")
			}
		}(i)
	}

	wg.Wait()
	le.Flush()
}
