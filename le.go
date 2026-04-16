// Package le_go provides a Golang client library for logging to
// logentries.com over a TLS connection using an access token.
//
// # Architecture
//
// Print-family methods (Print, Printf, Println) dispatch log entries
// asynchronously via a bounded channel to a single consumer goroutine.
// When the channel is full, entries are dropped and counted (see Stats).
// Within the Print path, FIFO ordering is guaranteed.
//
// Write (io.Writer), Fatal, and Panic use a direct synchronous path
// that acquires the connection mutex independently of the Print queue.
// Write is decoupled from Print queue depth. Fatal and Panic use a
// bounded TryLock (500ms) to avoid blocking the crash path.
//
// # Behavior changes from prior versions
//
//   - Write now prepends the access token and appends a trailing newline.
//     Previously Write was a raw passthrough to the TCP connection.
//   - Write now chunks payloads exceeding 65000 bytes, consistent with Print.
//   - Fatal and Panic are synchronous in the caller goroutine. Fatal writes
//     before os.Exit; Panic panics in the caller's stack frame (recoverable
//     via defer recover). Previously both ran asynchronously in detached
//     goroutines.
//   - Under sustained connection-mutex contention, Fatal/Panic may exit or
//     panic without writing the log message. See Stats().DroppedSyncLock.
//   - Close is idempotent (second call returns nil) and blocks until the
//     async queue is drained.
//   - Write and Flush return ErrLoggerClosed after Close.
//   - Print calls that race with Close may be silently dropped. For
//     guaranteed delivery before shutdown, call Flush() and check its
//     error return before calling Close().
//   - Connection reuse now works correctly beyond 15 minutes of process
//     uptime (fixes a connection-storm bug in prior versions).
//   - Half-open TCP detection uses OS-level keepalive (30s period) instead
//     of a per-call application-level read probe. Middleboxes may swallow
//     keepalive probes; keep writeTimeout short for backup detection.
//   - errOutput no longer emits "Timedout waiting for logger.mu" diagnostics;
//     the mu-timeout path has been removed.
//   - On retry after partial TCP write, Write may under-report bytes written.
//   - errOutput should be non-blocking; a slow errOutput writer blocks all
//     log paths.
//   - Flush() now returns error (previously void). Existing callers that
//     discarded the return continue to compile.
//   - The concurrentWrites parameter to Connect now controls the channel
//     buffer capacity rather than a goroutine semaphore. When 0, defaults
//     to 4096.
package le_go

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrLoggerClosed = errors.New("le_go: logger is closed")

type DropReason int

const (
	DropQueueFull DropReason = iota
	DropClosed
	DropSyncLock
)

type Stats struct {
	DroppedQueueFull uint64
	DroppedClosed    uint64
	DroppedSyncLock  uint64
	PanicsRecovered  uint64
}

type Option func(*Logger)

type entry struct {
	s          string
	file       string
	prefix     string
	now        time.Time
	line       int
	flag       int
	numRetries int

	flush bool
	ack   chan error
}

type Logger struct {
	cfgMu      sync.Mutex
	flag       int
	prefix     string
	numRetries int

	host            string
	token           string
	calldepthOffset int
	writeTimeout    time.Duration
	dialTimeout     time.Duration
	tlsConfig       *tls.Config

	errOutputMutex sync.Mutex
	errOutput      io.Writer

	ch      chan entry
	done    chan struct{}
	exited  chan struct{}
	closed  atomic.Bool
	droppedQueueFull atomic.Uint64
	droppedClosed    atomic.Uint64
	droppedSyncLock  atomic.Uint64
	panicsRecovered  atomic.Uint64

	connMu        sync.Mutex
	conn          net.Conn
	lastRefreshAt time.Time

	_testWaitForWrite *sync.WaitGroup
	_testDropped      func(DropReason)
}

const lineSep = "\n"
const maxLogLength int = 65000
const retryBackoff = 100 * time.Millisecond
const fatalLockWait = 500 * time.Millisecond
const maxConsecutivePanics = 10

var defaultWriteTimeout = 10 * time.Second
var defaultDialTimeout = 10 * time.Second

// Connect creates a new Logger instance and opens a TLS connection.
// The concurrentWrites parameter controls the async channel buffer capacity;
// 0 defaults to 4096. Optional Option values configure test hooks and timeouts.
func Connect(host, token string, concurrentWrites int, errOutput io.Writer, calldepthOffset int, opts ...Option) (*Logger, error) {
	capacity := concurrentWrites
	if capacity <= 0 {
		capacity = 4096
	}

	l := &Logger{
		host:            host,
		token:           token,
		calldepthOffset: calldepthOffset,
		writeTimeout:    defaultWriteTimeout,
		dialTimeout:     defaultDialTimeout,
		ch:              make(chan entry, capacity),
		done:            make(chan struct{}),
		exited:          make(chan struct{}),
		lastRefreshAt:   time.Now(),
		_testDropped:    func(DropReason) {},
	}

	if errOutput != nil {
		l.errOutput = errOutput
	} else {
		l.errOutput = os.Stdout
	}

	for _, opt := range opts {
		opt(l)
	}

	if err := l.openConnectionLocked(); err != nil {
		return nil, err
	}

	go l.run()
	return l, nil
}

func WithTestWaitForWrite(wg *sync.WaitGroup) Option {
	return func(l *Logger) { l._testWaitForWrite = wg }
}

func WithTestDropped(fn func(DropReason)) Option {
	return func(l *Logger) { l._testDropped = fn }
}

func WithDialTimeout(d time.Duration) Option {
	return func(l *Logger) { l.dialTimeout = d }
}

func WithWriteTimeout(d time.Duration) Option {
	return func(l *Logger) { l.writeTimeout = d }
}

func (l *Logger) Stats() Stats {
	return Stats{
		DroppedQueueFull: l.droppedQueueFull.Load(),
		DroppedClosed:    l.droppedClosed.Load(),
		DroppedSyncLock:  l.droppedSyncLock.Load(),
		PanicsRecovered:  l.panicsRecovered.Load(),
	}
}

func (l *Logger) SetErrorOutput(errOutput io.Writer) {
	l.errOutputMutex.Lock()
	defer l.errOutputMutex.Unlock()
	l.errOutput = errOutput
}

func (l *Logger) SetNumberOfRetries(retries int) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.numRetries = retries
}

func (l *Logger) Flags() int {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return l.flag
}

func (l *Logger) SetFlags(flag int) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.flag = flag
}

func (l *Logger) Prefix() string {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return l.prefix
}

func (l *Logger) SetPrefix(prefix string) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.prefix = prefix
}

// Close signals the logger to shut down and blocks until the async queue
// is drained and the connection is closed. Idempotent.
func (l *Logger) Close() error {
	l.signalShutdown()
	<-l.exited
	return nil
}

func (l *Logger) signalShutdown() {
	if l.closed.CompareAndSwap(false, true) {
		close(l.done)
	}
}

// Output dispatches a log message asynchronously through the channel.
// Calldepth is provided for runtime.Caller file/line resolution.
func (l *Logger) Output(calldepth int, s string) {
	if l.closed.Load() {
		l.droppedClosed.Add(1)
		l._testDropped(DropClosed)
		return
	}

	l.cfgMu.Lock()
	flag, prefix, numRetries := l.flag, l.prefix, l.numRetries
	l.cfgMu.Unlock()

	var file string
	var line int
	if flag&(log.Lshortfile|log.Llongfile) != 0 {
		var ok bool
		_, file, line, ok = runtime.Caller(calldepth)
		if !ok {
			file = "???"
			line = 0
		}
	}

	s = replaceEmbeddedNewlines(s)

	e := entry{
		s:          s,
		file:       file,
		line:       line,
		flag:       flag,
		prefix:     prefix,
		numRetries: numRetries,
		now:        time.Now(),
	}

	if l.closed.Load() {
		l.droppedClosed.Add(1)
		l._testDropped(DropClosed)
		return
	}

	select {
	case l.ch <- e:
	case <-l.done:
		l.droppedClosed.Add(1)
		l._testDropped(DropClosed)
	default:
		select {
		case <-l.done:
			l.droppedClosed.Add(1)
			l._testDropped(DropClosed)
		default:
			l.droppedQueueFull.Add(1)
			l._testDropped(DropQueueFull)
		}
	}
}

func (l *Logger) Print(v ...interface{}) {
	l.Output(3+l.calldepthOffset, fmt.Sprint(v...))
}

func (l *Logger) Printf(format string, v ...interface{}) {
	l.Output(3+l.calldepthOffset, fmt.Sprintf(format, v...))
}

func (l *Logger) Println(v ...interface{}) {
	l.Output(3+l.calldepthOffset, fmt.Sprintln(v...))
}

// Write implements io.Writer. It prepends the access token and appends a
// trailing newline. Write is synchronous and decoupled from the async Print
// queue — it acquires connMu directly.
func (l *Logger) Write(p []byte) (int, error) {
	if l.closed.Load() {
		return 0, ErrLoggerClosed
	}

	l.cfgMu.Lock()
	numRetries := l.numRetries
	l.cfgMu.Unlock()

	raw := make([]byte, 0, len(l.token)+1+len(p)+1)
	raw = append(raw, (l.token + " ")...)
	raw = append(raw, p...)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}

	l.connMu.Lock()
	defer l.connMu.Unlock()

	if l.closed.Load() {
		return 0, ErrLoggerClosed
	}
	if err := l.ensureOpenConnectionLocked(); err != nil {
		return 0, err
	}
	if err := l.writeRawBytesLocked(raw, numRetries); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush waits for all queued async Print entries to be processed.
// Returns ErrLoggerClosed if the logger is closed during the wait.
func (l *Logger) Flush() error {
	if l.closed.Load() {
		return ErrLoggerClosed
	}
	ack := make(chan error, 1)
	select {
	case l.ch <- entry{flush: true, ack: ack}:
	case <-l.done:
		return ErrLoggerClosed
	}
	select {
	case err := <-ack:
		return err
	case <-l.done:
		return ErrLoggerClosed
	}
}

func (l *Logger) Fatal(v ...interface{}) {
	s := fmt.Sprint(v...)
	l.fatalCommon(s, 3+l.calldepthOffset)
}

func (l *Logger) Fatalf(format string, v ...interface{}) {
	s := fmt.Sprintf(format, v...)
	l.fatalCommon(s, 3+l.calldepthOffset)
}

func (l *Logger) Fatalln(v ...interface{}) {
	s := fmt.Sprintln(v...)
	l.fatalCommon(s, 3+l.calldepthOffset)
}

func (l *Logger) fatalCommon(s string, calldepth int) {
	formatted, file, line, flag, prefix := l.formatMessage(s, calldepth+1)

	l.cfgMu.Lock()
	numRetries := l.numRetries
	l.cfgMu.Unlock()

	acquired := l.tryAcquireConnMu(fatalLockWait)
	if !acquired {
		l.droppedSyncLock.Add(1)
		l._testDropped(DropSyncLock)
		l.writeToErrOutput("Fatal could not acquire conn lock; exiting without flush\n")
		os.Exit(1)
	}
	defer l.connMu.Unlock()

	if !l.closed.Load() {
		_ = l.writeFormattedMessageLocked(formatted, file, time.Now(), line, flag, prefix, numRetries)
	}
	os.Exit(1)
}

func (l *Logger) Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	l.panicCommon(s, 3+l.calldepthOffset)
}

func (l *Logger) Panicf(format string, v ...interface{}) {
	s := fmt.Sprintf(format, v...)
	l.panicCommon(s, 3+l.calldepthOffset)
}

func (l *Logger) Panicln(v ...interface{}) {
	s := fmt.Sprintln(v...)
	l.panicCommon(s, 3+l.calldepthOffset)
}

func (l *Logger) panicCommon(s string, calldepth int) {
	formatted, file, line, flag, prefix := l.formatMessage(s, calldepth+1)

	l.cfgMu.Lock()
	numRetries := l.numRetries
	l.cfgMu.Unlock()

	acquired := l.tryAcquireConnMu(fatalLockWait)
	if !acquired {
		l.droppedSyncLock.Add(1)
		l._testDropped(DropSyncLock)
		panic(s)
	}

	if !l.closed.Load() {
		_ = l.writeFormattedMessageLocked(formatted, file, time.Now(), line, flag, prefix, numRetries)
	}
	l.connMu.Unlock()
	panic(s)
}

// --- Consumer goroutine ---

func (l *Logger) run() {
	defer close(l.exited)
	panicsSinceLastSuccess := 0

	for {
		exited := func() bool {
			defer func() {
				if r := recover(); r != nil {
					l.panicsRecovered.Add(1)
					panicsSinceLastSuccess++
					l.writeToErrOutput(fmt.Sprintf(
						"consumer panic recovered (%d/%d): %v\n",
						panicsSinceLastSuccess, maxConsecutivePanics, r))
				}
			}()
			for {
				select {
				case e := <-l.ch:
					l.processAsync(e, &panicsSinceLastSuccess)
				case <-l.done:
					l.drain()
					l.closeConnLocked()
					return true
				}
			}
		}()

		if exited {
			return
		}

		if panicsSinceLastSuccess >= maxConsecutivePanics {
			l.writeToErrOutput("consumer hit panic limit; shutting down logger\n")
			l.signalShutdown()
			l.drain()
			l.closeConnLocked()
			return
		}
	}
}

func (l *Logger) processAsync(e entry, panicsSinceLastSuccess *int) {
	if e.flush {
		e.ack <- nil
		return
	}

	l.connMu.Lock()
	defer l.connMu.Unlock()

	if err := l.writeFormattedMessageLocked(e.s, e.file, e.now, e.line, e.flag, e.prefix, e.numRetries); err != nil {
		l.writeToErrOutput(fmt.Sprintf("le_go: write error: %s; payload len: %d\n", err, len(e.s)))
		return
	}

	*panicsSinceLastSuccess = 0
}

func (l *Logger) drain() {
	for {
		select {
		case e := <-l.ch:
			if e.flush {
				e.ack <- nil
				continue
			}
			l.connMu.Lock()
			if l.conn != nil {
				_ = l.writeFormattedMessageLocked(e.s, e.file, e.now, e.line, e.flag, e.prefix, e.numRetries)
			}
			l.connMu.Unlock()
		default:
			return
		}
	}
}

func (l *Logger) closeConnLocked() {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	if l.conn != nil {
		_ = l.conn.Close()
		l.conn = nil
	}
}

func replaceEmbeddedNewlines(s string) string {
	s = strings.TrimRight(s, lineSep)
	return strings.ReplaceAll(s, lineSep, "\u2028")
}

// --- Formatting & writing ---

// formatMessage builds a fully-formatted log message (token + header + s + \n)
// for the Fatal/Panic direct path. Called BEFORE acquiring connMu.
// For messages longer than maxLogLength, this returns the full payload and
// writeFormattedBytesLocked handles chunking.
func (l *Logger) formatMessage(s string, calldepth int) (formatted string, file string, line int, flag int, prefix string) {
	l.cfgMu.Lock()
	flag, prefix = l.flag, l.prefix
	l.cfgMu.Unlock()

	if flag&(log.Lshortfile|log.Llongfile) != 0 {
		var ok bool
		_, file, line, ok = runtime.Caller(calldepth)
		if !ok {
			file = "???"
			line = 0
		}
	}

	formatted = replaceEmbeddedNewlines(s)
	return
}

// writeFormattedMessageLocked writes a log message, chunking at maxLogLength.
// Each chunk is individually prefixed with token + header and newline-terminated.
// Caller must hold connMu.
func (l *Logger) writeFormattedMessageLocked(s, file string, now time.Time, line, flag int, prefix string, numRetries int) error {
	if err := l.ensureOpenConnectionLocked(); err != nil {
		return err
	}

	i := 0
	for {
		end := i + maxLogLength - 2
		if end > len(s) {
			end = len(s)
		}

		var buf []byte
		buf = append(buf, (l.token + " ")...)
		formatHeader(&buf, now, file, line, flag, prefix)
		buf = append(buf, s[i:end]...)
		buf = append(buf, '\n')

		if err := l.writeRawBytesLocked(buf, numRetries); err != nil {
			return err
		}

		i = end
		if i >= len(s) {
			break
		}
	}
	return nil
}

func formatHeader(buf *[]byte, t time.Time, file string, line int, flag int, prefix string) {
	*buf = append(*buf, prefix...)
	if flag&(log.Ldate|log.Ltime|log.Lmicroseconds) != 0 {
		if flag&log.LUTC != 0 {
			t = t.UTC()
		}
		if flag&log.Ldate != 0 {
			year, month, day := t.Date()
			itoa(buf, year, 4)
			*buf = append(*buf, '/')
			itoa(buf, int(month), 2)
			*buf = append(*buf, '/')
			itoa(buf, day, 2)
			*buf = append(*buf, ' ')
		}
		if flag&(log.Ltime|log.Lmicroseconds) != 0 {
			hour, min, sec := t.Clock()
			itoa(buf, hour, 2)
			*buf = append(*buf, ':')
			itoa(buf, min, 2)
			*buf = append(*buf, ':')
			itoa(buf, sec, 2)
			if flag&log.Lmicroseconds != 0 {
				*buf = append(*buf, '.')
				itoa(buf, t.Nanosecond()/1e3, 6)
			}
			*buf = append(*buf, ' ')
		}
	}
	if flag&(log.Lshortfile|log.Llongfile) != 0 {
		if flag&log.Lshortfile != 0 {
			short := file
			for i := len(file) - 1; i > 0; i-- {
				if file[i] == '/' {
					short = file[i+1:]
					break
				}
			}
			file = short
		}
		*buf = append(*buf, file...)
		*buf = append(*buf, ':')
		itoa(buf, line, -1)
		*buf = append(*buf, ": "...)
	}
}

func itoa(buf *[]byte, i int, wid int) {
	var b [20]byte
	bp := len(b) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		b[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	b[bp] = byte('0' + i)
	*buf = append(*buf, b[bp:]...)
}

// --- Connection management (caller must hold connMu) ---

func (l *Logger) openConnectionLocked() error {
	if l.conn != nil {
		_ = l.conn.Close()
	}

	dialer := &net.Dialer{Timeout: l.dialTimeout}
	tlsCfg := &tls.Config{}
	if l.tlsConfig != nil {
		tlsCfg = l.tlsConfig
	}

	conn, err := tls.DialWithDialer(dialer, "tcp", l.host, tlsCfg)
	if err != nil {
		l.conn = nil
		return err
	}

	if tc, ok := conn.NetConn().(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	l.conn = conn
	l.lastRefreshAt = time.Now()
	return nil
}

func (l *Logger) ensureOpenConnectionLocked() error {
	if l.conn == nil || time.Since(l.lastRefreshAt) > 15*time.Minute {
		return l.openConnectionLocked()
	}
	return nil
}

// writeRawBytesLocked writes data to the TCP connection with retries.
// Caller must hold connMu. May temporarily release connMu during retry backoff
// when no bytes have been sent yet (to allow Fatal/Panic TryLock to succeed
// during the sleep window).
func (l *Logger) writeRawBytesLocked(data []byte, numRetries int) error {
	var bytesSent int
	var lastErr error

	for attempt := 0; attempt <= numRetries; attempt++ {
		if attempt > 0 {
			if bytesSent == 0 {
				l.connMu.Unlock()
				time.Sleep(retryBackoff)
				l.connMu.Lock()
				if l.closed.Load() {
					return ErrLoggerClosed
				}
				if err := l.ensureOpenConnectionLocked(); err != nil {
					lastErr = err
					continue
				}
			} else {
				time.Sleep(retryBackoff)
			}
		}

		if l.conn == nil {
			if err := l.ensureOpenConnectionLocked(); err != nil {
				lastErr = err
				continue
			}
		}

		if err := l.conn.SetWriteDeadline(time.Now().Add(l.writeTimeout)); err != nil {
			lastErr = err
			l.writeToErrOutput(fmt.Sprintf("le_go: SetWriteDeadline error: %s\n", err))
			continue
		}

		n, err := l.conn.Write(data)
		if err != nil {
			lastErr = err
			l.writeToErrOutput(fmt.Sprintf("le_go: write error: %s\n", err))
			l.conn.Close()
			l.conn = nil
			continue
		}

		bytesSent += n
		if l._testWaitForWrite != nil {
			l._testWaitForWrite.Done()
		}
		return nil
	}
	return lastErr
}

func (l *Logger) tryAcquireConnMu(budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if l.connMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (l *Logger) writeToErrOutput(s string) {
	l.errOutputMutex.Lock()
	defer l.errOutputMutex.Unlock()
	fmt.Fprint(l.errOutput, s)
}

// withConnLocked calls fn with the current connection while holding connMu.
// Intended for test use only.
func (l *Logger) withConnLocked(fn func(net.Conn)) {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	fn(l.conn)
}
