// Package le_go provides a Golang client library for logging to
// logentries.com over a TCP connection.
//
// it uses an access token for sending log events.
package le_go

import (
	"crypto/tls"
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
	errOutputMutex   *sync.RWMutex
	numRetries       int
	closed           int32 // atomic: 0=open, 1=closed
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

	return &logger, nil
}

func newEmptyLogger(host, token string, calldepthOffset int) Logger {
	return Logger{
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
		errOutputMutex:     &sync.RWMutex{},
	}
}

func (logger *Logger) SetErrorOutput(errOutput io.Writer) {
	logger.errOutputMutex.Lock()
	defer logger.errOutputMutex.Unlock()
	logger.errOutput = errOutput
}

// Close flushes pending writes and closes the TCP connection.
func (logger *Logger) Close() error {
	atomic.StoreInt32(&logger.closed, 1)
	// Barrier: any Output() that already passed the closed check under mu
	// has done wg.Add(1) before releasing mu. After we acquire+release mu,
	// no new Add(1) can happen, so wg.Wait is safe.
	logger.mu.Lock()
	logger.mu.Unlock()
	logger.wg.Wait()

	logger.connMu.Lock()
	defer logger.connMu.Unlock()
	if logger.conn != nil {
		return logger.conn.Close()
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

// writeToErrOutput writes to the error output, ensuring it is not being modified concurrently.
func (l *Logger) writeToErrOutput(s string) {
	l.errOutputMutex.RLock()
	defer l.errOutputMutex.RUnlock()
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
	defer func() {
		if re := recover(); re != nil {
			l.writeToErrOutput(fmt.Sprintf("Panicked in logger.output %v\n", re))
			debug.PrintStack()
			panic(re)
		}
	}()

	if l.concurrentWrites != nil {
		select {
		case <-l.concurrentWrites:
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
	if atomic.LoadInt32(&l.closed) != 0 {
		l.mu.Unlock()
		if l.concurrentWrites != nil {
			l.concurrentWrites <- struct{}{}
		}
		return
	}
	l.wg.Add(1)
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
	defer func() {
		if re := recover(); re != nil {
			//Protect against misused waitgroups
			//Usually won't be an issue if the Flush is not waiting a long time
			log.Println("Recovered while flushing logs")
		}
	}()
	// Barrier: ensure any in-flight Output() that passed closed check has
	// completed wg.Add(1) before we call wg.Wait.
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
func (logger *Logger) Write(p []byte) (n int, err error) {
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
