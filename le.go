// Package le_go provides a Golang client library for logging to
// logentries.com over a TCP connection.
//
// it uses an access token for sending log events.
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
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrLoggerClosed is returned by Write when called on a Logger that has been
// closed. It indicates the call was rejected and no bytes were written.
var ErrLoggerClosed = errors.New("le_go: logger is closed")

// Logger represents a Logentries logger,
// it holds the open TCP connection, access token, prefix and flags.
//
// all Logger operations are thread safe and blocking,
// log operations can be invoked in a non-blocking way by calling them from
// a goroutine.
type Logger struct {
	conn             net.Conn
	flag             int
	mu               *sync.Mutex   // protects flag, prefix
	connMu           *sync.Mutex   // protects conn, lastRefreshAt, buf; serializes writes
	concurrentWrites chan struct{}  // semaphore; nil when unlimited
	calldepthOffset  int
	prefix           string
	host             string
	token            string
	buf              []byte
	lastRefreshAt    time.Time
	writeTimeout     time.Duration
	dialTimeout      time.Duration
	tlsConfig        *tls.Config
	_testWaitForWrite  *sync.WaitGroup
	_testTimedoutWrite func()
	wg               *sync.WaitGroup
	errOutput        io.Writer
	errOutputMutex   *sync.Mutex
	numRetries       int
	closed           atomic.Bool
}

const lineSep = "\n"
const maxLogLength int = 65000 //add 535 chars of headroom for the filename, timestamp and header
var defaultWriteTimeout = 10 * time.Second
var defaultDialTimeout = 10 * time.Second

// Connect creates a new Logger instance and opens a TCP connection to
// logentries.com,
// The token can be generated at logentries.com by adding a new log,
// choosing manual configuration and token based TCP connection.
func Connect(host, token string, concurrentWrites int, errOutput io.Writer, calldepthOffset int) (*Logger, error) {
	logger := newEmptyLogger(host, token, calldepthOffset)
	if concurrentWrites > 0 {
		logger.concurrentWrites = make(chan struct{}, concurrentWrites)
		for i := 0; i < concurrentWrites; i++ {
			logger.concurrentWrites <- struct{}{}
		}
	}
	if errOutput != nil {
		logger.errOutput = errOutput
	} else {
		logger.errOutput = os.Stdout
	}

	if err := logger.openConnection(); err != nil {
		return nil, err
	}

	return logger, nil
}

func newEmptyLogger(host, token string, calldepthOffset int) *Logger {
	return &Logger{
		host:               host,
		token:              token,
		calldepthOffset:    calldepthOffset,
		lastRefreshAt:      time.Now(),
		writeTimeout:       defaultWriteTimeout,
		dialTimeout:        defaultDialTimeout,
		mu:                 &sync.Mutex{},
		connMu:             &sync.Mutex{},
		_testTimedoutWrite: func() {}, //NOP for prod
		wg:                 &sync.WaitGroup{},
		errOutputMutex:     &sync.Mutex{},
	}
}

func (logger *Logger) SetErrorOutput(errOutput io.Writer) {
	logger.errOutputMutex.Lock()
	defer logger.errOutputMutex.Unlock()
	logger.errOutput = errOutput
}

// Close flushes pending writes and closes the TCP connection.
func (logger *Logger) Close() error {
	logger.closed.Store(true)
	// Barrier: any Output()/Write() that already passed the closed check
	// under mu has done wg.Add(1) before releasing mu. After we acquire
	// and release mu, no new Add(1) can happen, so wg.Wait is safe.
	logger.mu.Lock()
	logger.mu.Unlock()
	logger.wg.Wait()

	logger.connMu.Lock()
	defer logger.connMu.Unlock()
	if logger.conn != nil {
		err := logger.conn.Close()
		logger.conn = nil
		return err
	}
	return nil
}

// openConnection opens a new TLS connection, closing any existing one first.
// Caller must hold connMu (except during initial Connect).
func (logger *Logger) openConnection() error {
	if logger.conn != nil {
		logger.conn.Close()
	}

	dialer := &net.Dialer{Timeout: logger.dialTimeout}
	tlsCfg := &tls.Config{}
	if logger.tlsConfig != nil {
		tlsCfg = logger.tlsConfig
	}

	conn, err := tls.DialWithDialer(dialer, "tcp", logger.host, tlsCfg)
	if err != nil {
		return err
	}
	logger.conn = conn
	logger.lastRefreshAt = time.Now()
	return nil
}

// ensureOpenConnection ensures the TCP connection is open.
// If the connection is nil or stale (>15 min), it is refreshed.
// Caller must hold connMu.
func (logger *Logger) ensureOpenConnection() error {
	if logger.conn == nil || time.Now().After(logger.lastRefreshAt.Add(15*time.Minute)) {
		return logger.openConnection()
	}
	return nil
}

// Fatal is same as Print() but calls to os.Exit(1)
func (logger *Logger) Fatal(v ...interface{}) {
	logger.Output(3+logger.calldepthOffset, fmt.Sprint(v...), handleFatalActions)
}

// Fatalf is same as Printf() but calls to os.Exit(1)
func (logger *Logger) Fatalf(format string, v ...interface{}) {
	logger.Output(3+logger.calldepthOffset, fmt.Sprintf(format, v...), handleFatalActions)
}

// Fatalln is same as Println() but calls to os.Exit(1)
func (logger *Logger) Fatalln(v ...interface{}) {
	logger.Output(3+logger.calldepthOffset, fmt.Sprintln(v...), handleFatalActions)
}

// Flags returns the logger flags
func (logger *Logger) Flags() int {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.flag
}

// writeToErrOutput writes to the error output, ensuring it is not being
// modified concurrently. Holds the mutex for the duration of the Fprint so
// that arbitrary user-supplied io.Writers (which may not themselves be
// goroutine-safe) are not written to concurrently.
func (l *Logger) writeToErrOutput(s string) {
	l.errOutputMutex.Lock()
	defer l.errOutputMutex.Unlock()
	fmt.Fprint(l.errOutput, s)
}

// Output writes the output for a logging event. The string s contains
// the text to print after the prefix specified by the flags of the
// Logger. A newline is appended if the last character of s is not
// already a newline. Calldepth is used to recover the PC and is
// provided for generality, although at the moment on all pre-defined
// paths it will be 3 plus a given offset.
// Output does the actual writing to the TCP connection
func (l *Logger) Output(calldepth int, s string, doAsync func()) {
	// tokenHeld and wgAdded track ownership of the semaphore token and the
	// wg counter so that the recover below can release them if a panic
	// occurs after acquisition but before ownership is handed to the
	// spawned goroutine. Without this, a panic in this region would leak
	// the wg counter and deadlock Close/Flush forever.
	var (
		tokenHeld bool
		wgAdded   bool
	)
	defer func() {
		if re := recover(); re != nil {
			if wgAdded {
				l.wg.Done()
			}
			if tokenHeld {
				l.concurrentWrites <- struct{}{}
			}
			l.writeToErrOutput(fmt.Sprintf("Panicked in logger.output %v\n", re))
			debug.PrintStack()
			panic(re)
		}
	}()

	if l.concurrentWrites != nil {
		select {
		case <-l.concurrentWrites:
			tokenHeld = true
		default:
			return
		}
	}

	now := time.Now() // get this early.
	var file string
	var line int

	// Hold mu for: closed check + wg.Add(1) + snapshot flag/prefix.
	// This prevents wg.Add from racing with wg.Wait in Close/Flush
	// (they acquire mu as a barrier before calling wg.Wait).
	l.mu.Lock()
	if l.closed.Load() {
		l.mu.Unlock()
		if tokenHeld {
			l.concurrentWrites <- struct{}{}
			tokenHeld = false
		}
		return
	}
	l.wg.Add(1)
	wgAdded = true
	flag := l.flag
	prefix := l.prefix
	l.mu.Unlock()

	if flag&(log.Lshortfile|log.Llongfile) != 0 {
		var ok bool
		_, file, line, ok = runtime.Caller(calldepth)
		if !ok {
			file = "???"
			line = 0
		}
	}

	// Replace embedded newlines with unicode line separator,
	// stripping trailing newline (writeMessage always adds one per chunk).
	s = replaceEmbeddedNewlines(s)

	// Hand ownership of the wg counter and semaphore token off to the
	// spawned goroutine. The `go` statement itself cannot panic, so this
	// hand-off is atomic with respect to the recover above.
	wgAdded = false
	tokenHeld = false
	go func() {
		defer l.wg.Done()
		if l.concurrentWrites != nil {
			defer func() { l.concurrentWrites <- struct{}{} }()
		}
		l.writeMessage(s, file, now, line, flag, prefix)
		doAsync()
	}()
}

// replaceEmbeddedNewlines strips trailing newlines and replaces all remaining
// newlines with the unicode line separator \u2028.
func replaceEmbeddedNewlines(s string) string {
	s = strings.TrimRight(s, lineSep)
	s = strings.ReplaceAll(s, lineSep, "\u2028")
	return s
}

func (l *Logger) Flush() {
	// Barrier: ensure any in-flight Output()/Write() that passed the closed
	// check has completed wg.Add(1) before we call wg.Wait. With the
	// barrier in place, wg.Wait cannot legitimately panic — every Add(1)
	// is performed under mu and is therefore ordered either fully-before
	// or fully-after this Wait.
	l.mu.Lock()
	l.mu.Unlock()
	l.wg.Wait()
}

// Panic is same as Print() but calls to panic
func (logger *Logger) Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	logger.Output(3+logger.calldepthOffset, s, handlePanicActions(s))
}

// Panicf is same as Printf() but calls to panic
func (logger *Logger) Panicf(format string, v ...interface{}) {
	s := fmt.Sprintf(format, v...)
	logger.Output(3+logger.calldepthOffset, s, handlePanicActions(s))
}

// Panicln is same as Println() but calls to panic
func (logger *Logger) Panicln(v ...interface{}) {
	s := fmt.Sprintln(v...)
	logger.Output(3+logger.calldepthOffset, s, handlePanicActions(s))
}

// Prefix returns the logger prefix
func (logger *Logger) Prefix() string {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.prefix
}

// Print logs a message
func (logger *Logger) Print(v ...interface{}) {
	logger.Output(3+logger.calldepthOffset, fmt.Sprint(v...), handlePrintActions)
}

// Printf logs a formatted message
func (logger *Logger) Printf(format string, v ...interface{}) {
	logger.Output(3+logger.calldepthOffset, fmt.Sprintf(format, v...), handlePrintActions)
}

// Println logs a message with a linebreak
func (logger *Logger) Println(v ...interface{}) {
	logger.Output(3+logger.calldepthOffset, fmt.Sprintln(v...), handlePrintActions)
}

// SetFlags sets the logger flags
func (logger *Logger) SetFlags(flag int) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.flag = flag
}

func (logger *Logger) SetNumberOfRetries(retries int) {
	logger.connMu.Lock()
	defer logger.connMu.Unlock()
	logger.numRetries = retries
}

// SetPrefix sets the logger prefix
func (logger *Logger) SetPrefix(prefix string) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.prefix = prefix
}

// Write writes a bytes array to the Logentries TCP connection,
// it adds the access token and also ensures newline termination.
//
// If the Logger has been closed, Write returns 0 and ErrLoggerClosed
// without touching the (closed) connection.
func (logger *Logger) Write(p []byte) (n int, err error) {
	// Honor the close barrier the same way Output does: take mu, check
	// closed, and wg.Add(1) under mu so that Close's wg.Wait will await
	// this in-flight write. Without this, Write could resurrect a closed
	// Logger by re-dialing inside writeRaw's retry loop.
	logger.mu.Lock()
	if logger.closed.Load() {
		logger.mu.Unlock()
		return 0, ErrLoggerClosed
	}
	logger.wg.Add(1)
	logger.mu.Unlock()
	defer logger.wg.Done()

	logger.connMu.Lock()
	defer logger.connMu.Unlock()

	var buf []byte
	buf = append(buf, (logger.token + " ")...)
	buf = append(buf, p...)
	if len(buf) == 0 || buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}

	if err := logger.writeRaw(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Taken wholesale from src/log/log.go
// formatHeader writes log header to buf in following order:
//   - l.prefix (if it's not blank),
//   - date and/or time (if corresponding flags are provided),
//   - file and line number (if corresponding flags are provided).
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

// Taken wholesale from src/log/log.go
// Cheap integer to fixed-width decimal ASCII. Give a negative width to avoid zero-padding.
func itoa(buf *[]byte, i int, wid int) {
	// Assemble decimal in reverse order.
	var b [20]byte
	bp := len(b) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		b[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	// i < 10
	b[bp] = byte('0' + i)
	*buf = append(*buf, b[bp:]...)
}

// writeMessage formats and sends a log message, chunking if necessary.
// Each chunk is prefixed with the token and header, and newline-terminated.
func (l *Logger) writeMessage(s, file string, now time.Time, line int, flag int, prefix string) {
	l.connMu.Lock()
	defer l.connMu.Unlock()

	i := 0
	for {
		end := i + maxLogLength - 2
		if end > len(s) {
			end = len(s)
		}
		l.buf = l.buf[:0]
		l.buf = append(l.buf, (l.token + " ")...)
		formatHeader(&l.buf, now, file, line, flag, prefix)
		l.buf = append(l.buf, s[i:end]...)
		l.buf = append(l.buf, '\n')

		if err := l.writeRaw(l.buf); err != nil {
			l.writeToErrOutput(fmt.Sprintf("le_go: write error: %s, wanted to log: %s\n", err, s))
			return
		}

		if l._testWaitForWrite != nil {
			l._testWaitForWrite.Done()
		}

		i = end
		if i >= len(s) {
			break
		}
	}
}

// writeRaw writes data to the TCP connection with retry logic.
// Caller must hold connMu.
func (l *Logger) writeRaw(data []byte) error {
	numAttempts := 1 + l.numRetries
	var err error
	for attempt := 0; attempt < numAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if err = l.ensureOpenConnection(); err != nil {
			continue
		}
		l.conn.SetWriteDeadline(time.Now().Add(l.writeTimeout))
		_, err = l.conn.Write(data)
		if err == nil {
			return nil
		}
		// Force reconnect on next attempt
		l.conn.Close()
		l.conn = nil
	}
	return err
}

func handleFatalActions() {
	os.Exit(1)
}

func handlePanicActions(s string) func() {
	return func() {
		panic(s)
	}
}

func handlePrintActions() {
	return
}
