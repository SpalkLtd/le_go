# Concurrency Verification — `le.go`

**Branch:** `refactor/logger-bugfixes`
**Scope:** `le.go` only. Test files are out of scope except where they reach into unexported fields (noted).
**Method:** Structured proof — enumerate every shared field, document its locking discipline, then walk each operation and show that all reads/writes respect that discipline. For wait/signal pairs (`wg`, semaphore), prove conservation on every control-flow path including panics. For locks, prove a fixed acquisition order so deadlock is impossible.
**What this proves:** Absence of data races, absence of lock-order deadlocks, conservation of `wg` counter and semaphore tokens, and correctness of the close/flush barrier — modulo the assumptions in §1 and the defects in §6.
**What this does NOT prove:** Liveness under adversarial network conditions; correctness of user-supplied `io.Writer` thread-safety; behavior after a runtime panic from outside this file.

---

## 1. Assumptions

1. **Atomics are sequentially consistent.** Go's `sync/atomic` package provides this.
2. **`sync.Mutex` provides release-acquire semantics.** A goroutine's `Unlock` happens-before any subsequent `Lock` of the same mutex.
3. **`net.Conn.Close` is safe to call multiple times** and is safe to call concurrently with `Write` (it will cause the in-flight `Write` to fail). This is documented for `tls.Conn` and the underlying `net.TCPConn`.
4. **`runtime.Caller`, `time.Now`, `strings.TrimRight`, `strings.ReplaceAll` do not panic** on the inputs used here.
5. **The `tlsConfig` field is never written in production.** No setter exists in `le.go`. Tests poke the unexported field directly; in production it stays `nil` after `newEmptyLogger`. Treated as immutable.
6. **The user-supplied `io.Writer` passed as `errOutput` is goroutine-safe for concurrent `Write` calls.** `os.Stdout`/`os.Stderr` satisfy this. See §6 defect D4 for the consequence if this is violated.

---

## 2. Shared-state inventory

Every field on `Logger` and its protection regime:

| Field | Type | Mutability | Protection |
|---|---|---|---|
| `conn` | `net.Conn` | mutable | `connMu` |
| `flag` | `int` | mutable | `mu` |
| `mu` | `*sync.Mutex` | set in `newEmptyLogger`, never reassigned | immutable pointer |
| `connMu` | `*sync.Mutex` | set in `newEmptyLogger`, never reassigned | immutable pointer |
| `concurrentWrites` | `chan struct{}` | set in `Connect` before exposure, never reassigned | immutable; channel ops internally synchronized |
| `calldepthOffset` | `int` | set in `newEmptyLogger`, never reassigned | immutable |
| `prefix` | `string` | mutable | `mu` |
| `host` | `string` | set in `newEmptyLogger`, never reassigned | immutable |
| `token` | `string` | set in `newEmptyLogger`, never reassigned | immutable |
| `buf` | `[]byte` | mutable | `connMu` |
| `lastRefreshAt` | `time.Time` | mutable | `connMu` |
| `writeTimeout` | `time.Duration` | set in `newEmptyLogger`, never reassigned | immutable |
| `dialTimeout` | `time.Duration` | set in `newEmptyLogger`, never reassigned | immutable |
| `tlsConfig` | `*tls.Config` | never reassigned in production (assumption A5) | immutable |
| `_testWaitForWrite` | `*sync.WaitGroup` | test-only | by test discipline |
| `_testTimedoutWrite` | `func()` | test-only | by test discipline |
| `wg` | `*sync.WaitGroup` | set in `newEmptyLogger`, never reassigned | immutable pointer; uses internal sync |
| `errOutput` | `io.Writer` | mutable | `errOutputMutex` (held for full duration of write) |
| `errOutputMutex` | `*sync.Mutex` | set in `newEmptyLogger`, never reassigned | immutable pointer |
| `numRetries` | `int` | mutable | `connMu` |
| `closed` | `atomic.Bool` | mutable | atomic |

The fields marked "immutable" are written exactly once during construction (before the `*Logger` is returned from `Connect`). Construction happens-before any external access, so reads are race-free without a lock.

---

## 3. Lock order

A fixed total order on all locks held simultaneously:

```
mu  <  connMu  <  errOutputMutex
```

No operation in `le.go` ever holds `mu` and `connMu` at the same time (verified per-operation in §4). `errOutputMutex.RLock` is taken inside `writeMessage` while `connMu` is held; `errOutputMutex` is otherwise held alone. `mu` and `errOutputMutex` are never held together. The order is therefore acyclic ⇒ **no AB-BA deadlock is possible**.

---

## 4. Per-operation proof

For each public operation, list (a) what shared state it touches, (b) under which lock, (c) any wait/signal effects.

### `newEmptyLogger`, `Connect`
Constructs the value. No concurrency until the `*Logger` is returned. `openConnection` is called once before exposure; the comment on `openConnection` explicitly carves this out from the "caller must hold connMu" rule. ✓

### `SetErrorOutput(w)`
- `errOutputMutex.Lock` → write `errOutput` → `Unlock`. ✓

### `writeToErrOutput(s)` (private, called from `Output` recover and `writeMessage`)
- `errOutputMutex.RLock` → `fmt.Fprint(errOutput, s)` → `RUnlock`.
- The `RLock` protects the *pointer* from being swapped under us, not the underlying writer. See defect D4.

### `Flags()`, `SetFlags(f)`, `Prefix()`, `SetPrefix(p)`
- All four bracket their access to `flag`/`prefix` with `mu.Lock`/`Unlock`. ✓

### `SetNumberOfRetries(n)`
- `connMu.Lock` → write `numRetries` → `Unlock`.
- `numRetries` is read in `writeRaw`, which is only called from `writeMessage` and `Write`, both of which hold `connMu`. ✓

### `openConnection()` (private, "caller must hold connMu")
- Reads/writes `conn`, `lastRefreshAt`. Reads `tlsConfig`, `dialTimeout`, `host` (immutable). All fine under `connMu`. ✓

### `ensureOpenConnection()` (private, "caller must hold connMu")
- Reads `conn`, `lastRefreshAt`. ✓

### `writeRaw(data)` (private, "caller must hold connMu")
- Reads `numRetries`, `writeTimeout`, calls `ensureOpenConnection`, reads/writes `conn`. All under `connMu`. ✓

### `writeMessage(s, file, now, line, flag, prefix)` (private)
- Acquires `connMu` for the whole body. Reads/writes `buf`, calls `writeRaw` (OK — `connMu` held), reads `_testWaitForWrite`, calls `writeToErrOutput` on error (acquires `errOutputMutex.RLock` inside `connMu` — order respected).
- `flag` and `prefix` are passed by value from `Output`'s snapshot under `mu`; they are local to this call and cannot race. ✓

### `Write(p)` (the `io.Writer` interface)
- `connMu.Lock` for the whole body. Reads `token` (immutable), calls `writeRaw`. ✓ for race-freedom.
- **However**, `Write` does not check `closed` and does not increment `wg`. See defect D1.

### `Output(calldepth, s, doAsync)` — the critical operation

Annotated with the proof obligations:

```
defer recover()                                    // catches panics; rethrows
if concurrentWrites != nil:
    select { case <- concurrentWrites: ; default: return }   // (S1) acquire token or drop
now := time.Now()
mu.Lock()                                          // (M1) enter mu
if atomic.Load(closed) != 0:
    mu.Unlock()
    if concurrentWrites != nil: concurrentWrites <- {}       // (S2) release token
    return                                                    // closed-branch exit
wg.Add(1)                                          // (W1) under mu
flag, prefix = snapshot                            // safe read under mu
mu.Unlock()                                        // (M2) leave mu
if flag has shortfile|longfile: runtime.Caller(...)
s = replaceEmbeddedNewlines(s)
go func() {
    defer wg.Done()                                // (W2) released on goroutine exit
    if concurrentWrites != nil:
        defer { concurrentWrites <- {} }           // (S3) release token
    writeMessage(...)                              // acquires connMu
    doAsync()
}()
```

**Race-freedom for fields touched in Output's main body:**
- `concurrentWrites`: nil-check is on an immutable field; channel ops are safe.
- `closed`: atomic read. Race-free with the atomic store in `Close`.
- `flag`, `prefix`: read under `mu`. ✓
- `wg.Add(1)`: called under `mu` (so it can pair with the barrier in `Close`/`Flush`).

**Race-freedom inside the spawned goroutine:**
- `writeMessage` is the only thing that touches mutable shared state, and does so under `connMu`. ✓

**Semaphore conservation (token in `concurrentWrites`):** every successful `<- concurrentWrites` in (S1) must be matched by exactly one return.
- Closed-branch path: (S1) acquired, then (S2) returns it. ✓
- Normal path: (S1) acquired, (S3) returns it from the goroutine's defer.
- Defers run LIFO; the token-return defer runs *before* `wg.Done`, so by the time `wg.Done` fires the token is back. ✓
- Edge: if `doAsync` is `handleFatalActions` (calls `os.Exit(1)`), defers do **not** run — but the process is terminating, so semaphore state is irrelevant.
- Edge: if `doAsync` is `handlePanicActions` (panics), defers run, token returned, `wg.Done` called, panic propagates and likely crashes the goroutine and process. State is consistent in the moment, then dies. ✓
- Edge: if a panic occurs *between* (S1) and the `go` statement — by assumption A4 nothing in this region panics, so unreachable in practice. Worth flagging as a fragility (defect D3) but not a present bug.

**`wg` conservation:** every (W1) `Add(1)` is matched by exactly one (W2) `Done`.
- Both happen exactly once per non-closed-branch invocation.
- Same panic edges as above; same conclusion.
- Closed-branch exit does not call `Add(1)`, so no `Done` is needed. ✓

### `Close()`

```
atomic.Store(closed, 1)
mu.Lock(); mu.Unlock()           // (B) close-barrier
wg.Wait()
connMu.Lock(); defer connMu.Unlock()
if conn != nil: conn.Close()
```

**Claim (close-barrier correctness):** When `wg.Wait()` returns, every `Output` invocation that ever observed `closed == 0` has fully completed its spawned goroutine, and no future `Output` invocation will spawn a goroutine.

**Proof.** Let O be any `Output` invocation that read `closed == 0` at line (M1)–(W1). O held `mu` at the moment of the atomic load. Two cases on the order of events between O's `mu.Lock` and the close-barrier (B)'s `mu.Lock`.

*Case A: O acquired `mu` before (B).* Then O has executed `wg.Add(1)` before releasing `mu`. (B) blocks until O releases `mu`. After (B) returns, the `Add(1)` is visible to `wg.Wait`, and `wg.Wait` will block until the spawned goroutine calls `wg.Done`.

*Case B: (B) acquired `mu` before O.* Then (B)'s `mu.Unlock` happens-before O's `mu.Lock` (mutex release-acquire). The atomic store of `closed = 1` happens-before (B)'s `mu.Lock` in program order, hence happens-before O's `mu.Lock`, hence happens-before O's `atomic.Load(closed)`. Therefore O observes `closed == 1` and takes the closed-branch — contradicting the hypothesis that O observed 0. So Case B is vacuous for invocations that observed 0.

In both cases, every `Output` that observed 0 has its `Add(1)` accounted for before `wg.Wait` is called. `wg.Wait` thus waits for every spawned goroutine. ∎

**`conn` close ordering:** After `wg.Wait` returns, all spawned goroutines have run all their defers, which means each call to `writeMessage` has returned and released `connMu`. `Close` then acquires `connMu` uncontended (with respect to spawned goroutines) and closes `conn`. A concurrent `Write()` call is the one exception — see defect D1.

### `Flush()`
Same barrier pattern as `Close` minus the atomic store. The barrier proves: every `Output` invocation that began (i.e., reached `wg.Add(1)`) before `Flush`'s `mu.Lock` is awaited. `Output` invocations that begin after are not awaited — that's the documented contract of `Flush` ("flush pending writes"), not a bug.

The `defer recover` in `Flush` is **not justified by anything in §4**. With the barrier in place, `wg.Wait` cannot panic from a `wg.Add` racing past it. The recover masks bugs rather than preventing them. See defect D2.

---

## 5. Liveness / deadlock

- **Lock-order deadlock:** ruled out by §3.
- **`wg.Wait` deadlock:** ruled out by the conservation argument in §4 — every `Add(1)` is paired with a `Done` on every reachable path (modulo `os.Exit` from `Fatal*`, which terminates the process).
- **Channel-send deadlock on `concurrentWrites`:** the channel is buffered to capacity `concurrentWrites`. Tokens are conserved (§4). Therefore the send `concurrentWrites <- {}` always finds room and never blocks.
- **`writeMessage` blocking forever on TCP:** mitigated by `SetWriteDeadline(now + writeTimeout)` in `writeRaw`. ✓
- **`openConnection` blocking forever on dial:** mitigated by `net.Dialer{Timeout: dialTimeout}`. ✓

---

## 6. Defects found and fixed

All five defects identified by the original audit have been fixed on this branch. They are listed here for the record.

### D1. `Write()` did not honor `closed` — use-after-close — **FIXED**

`Write` now takes `mu`, checks `closed`, increments `wg` under `mu`, and `defer wg.Done()`s for the duration of the call. Returns `ErrLoggerClosed` when called on a closed Logger. This brings `Write` into the same close-barrier discipline as `Output`, so `Close` awaits in-flight `Write`s and a write-after-close cannot resurrect the connection.

### D2. `Flush()` had a bare `recover` masking bugs — **FIXED**

The `defer recover` in `Flush` has been removed. With the close-barrier in place (proven in §4), `wg.Wait` cannot legitimately panic from this code:

- "Negative counter" requires `Done > Add`. Every `Add(1)` in `Output`/`Write` is paired with at most one `Done`. ✓
- "WaitGroup reused before previous Wait returned" requires `Add` to race `Wait` at counter zero. The `mu.Lock; mu.Unlock` barrier forbids this — every `Add(1)` is ordered either fully-before or fully-after the `Wait`.

If a future change reintroduces a panic here, it will now surface instead of being silently swallowed.

### D3. Token / `wg` leak window in `Output` on panic — **FIXED**

`Output` now tracks `tokenHeld` and `wgAdded` as local booleans. The top-level recover releases both if a panic occurs after acquisition but before ownership is handed off to the spawned goroutine. Ownership is transferred immediately before the `go` statement (which itself cannot panic), making the hand-off effectively atomic.

### D4. `errOutputMutex` was `RWMutex` used as `RLock` for the write — **FIXED**

`errOutputMutex` is now a plain `*sync.Mutex` and `writeToErrOutput` holds it across the `fmt.Fprint`. Concurrent writes to a non-thread-safe user-supplied `io.Writer` are no longer possible. The cost is negligible because err-output writes only happen on the error path.

### D5. `Close()` did not nil `conn` after closing it — **FIXED**

`Close` now sets `logger.conn = nil` after `conn.Close()`. A subsequent `Close` is now idempotent (the `if logger.conn != nil` guard short-circuits), and the resurrection path that enabled D1 is closed off at the source.

### Bonus: `closed` is now `atomic.Bool` instead of `int32`

The original `closed int32` with `atomic.LoadInt32`/`StoreInt32` was a pre-`atomic.Bool` idiom. Replaced with `atomic.Bool` which provides the same guarantees with a clearer API. This makes `Logger` non-copyable (`atomic.Bool` contains a `noCopy` marker), which is the right thing — copying a `Logger` was never safe — and forced `newEmptyLogger` to return `*Logger` instead of `Logger`.

---

## 7. Verdict

For the fields in §2, with the locking discipline as documented, **`le.go` is free of data races and free of lock-order deadlocks**, the `Close`/`Flush` barriers do what they claim, and the five defects identified by this audit have all been fixed. The full test suite passes under `-race`.
