package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"greenlight/internal/assert"
)

// TestRateLimit turns the limiter on (it's off for the other tests) and
// confirms it actually rejects a flood.
func TestRateLimit(t *testing.T) {
	app, _ := newTestApplication(t)

	// 2 requests per second with a burst of 3: the first 3 go through
	// immediately, and the 4th should be rejected because the bucket is empty
	// and less than half a second has passed.
	app.config.limiter.enabled = true
	app.config.limiter.rps = 2
	app.config.limiter.burst = 3

	ts := newTestServer(t, app.routes())

	// Drain the burst.
	for i := range 3 {
		code, _, _ := ts.get(t, "/v1/healthcheck", "")
		if code != http.StatusOK {
			t.Fatalf("request %d: got %d; want 200 (still within the burst)", i+1, code)
		}
	}

	// The next one must be rejected.
	code, _, body := ts.get(t, "/v1/healthcheck", "")
	assert.Equal(t, code, http.StatusTooManyRequests)
	assert.StringContains(t, body, "rate limit exceeded")
}

// TestRateLimitDisabled is the control for the test above — it proves the
// `enabled` flag is actually consulted, rather than the limiter simply never
// firing.
func TestRateLimitDisabled(t *testing.T) {
	app, _ := newTestApplication(t)
	app.config.limiter.enabled = false

	ts := newTestServer(t, app.routes())

	for i := range 20 {
		code, _, _ := ts.get(t, "/v1/healthcheck", "")
		if code != http.StatusOK {
			t.Fatalf("request %d: got %d; want 200 with the limiter disabled", i+1, code)
		}
	}
}

// TestRateLimitIsPerClient checks that one client exhausting its bucket doesn't
// affect anyone else.
//
// This is the part of the limiter that carries real risk. The map of
// per-IP limiters and the realip lookup exist precisely so that a single noisy
// client can't lock everyone out — but the existing tests all come from one
// address, so a limiter that was accidentally global would pass them all and
// take the whole API down in production the first time one client misbehaved.
//
// realip.FromRequest reads X-Forwarded-For, which is what lets a test present
// itself as several different clients over one connection.
func TestRateLimitIsPerClient(t *testing.T) {
	app, _ := newTestApplication(t)

	app.config.limiter.enabled = true
	app.config.limiter.rps = 1
	app.config.limiter.burst = 2

	ts := newTestServer(t, app.routes())

	// Client A burns its whole burst and then gets rejected.
	for i := range 2 {
		code := getAsClient(t, ts, "/v1/healthcheck", "203.0.113.1")
		if code != http.StatusOK {
			t.Fatalf("client A request %d: got %d; want 200 (within burst)", i+1, code)
		}
	}

	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", "203.0.113.1"), http.StatusTooManyRequests)

	// Client B must be completely unaffected — a fresh bucket of its own.
	for i := range 2 {
		code := getAsClient(t, ts, "/v1/healthcheck", "198.51.100.7")
		if code != http.StatusOK {
			t.Fatalf("client B request %d: got %d; want 200 — B's bucket must be independent of A's", i+1, code)
		}
	}

	// And B can be exhausted independently, without reviving A.
	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", "198.51.100.7"), http.StatusTooManyRequests)
	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", "203.0.113.1"), http.StatusTooManyRequests)

	// A third client still gets through.
	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", "192.0.2.55"), http.StatusOK)
}

// TestRateLimitRefills checks that a client recovers once its bucket refills.
//
// A limiter that rejected a client permanently would pass every other test in
// this file, since none of them wait. The rate is set high enough that the
// pause is a few milliseconds of real time rather than a full second.
func TestRateLimitRefills(t *testing.T) {
	app, _ := newTestApplication(t)

	app.config.limiter.enabled = true
	// 100 requests/second means one token every 10ms.
	app.config.limiter.rps = 100
	app.config.limiter.burst = 1

	ts := newTestServer(t, app.routes())

	const ip = "203.0.113.99"

	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", ip), http.StatusOK)
	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", ip), http.StatusTooManyRequests)

	// Wait for the bucket to refill. Generous relative to the 10ms interval, so
	// a loaded CI machine doesn't produce a false failure.
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, getAsClient(t, ts, "/v1/healthcheck", ip), http.StatusOK)
}

// ─────────────────────────────────────────────────────────────────────────────
// THE RATE LIMITER'S JANITOR
// ─────────────────────────────────────────────────────────────────────────────
//
// rateLimit keeps a map of one *rate.Limiter per client IP, and a background
// goroutine sweeps entries that haven't been seen for three minutes. Without
// that sweep the map grows by one entry per IP that ever connects, which on a
// public API is a trivial way to exhaust the server's memory.
//
// Both tests below run inside a synctest bubble, for two different reasons.
//
// The first needs FAKE TIME: the janitor wakes once a minute and evicts after
// three, so testing it for real would mean a four-minute test. Inside a bubble
// the clock only advances when every goroutine is blocked, so four minutes pass
// instantly and deterministically.
//
// The second needs the bubble's GOROUTINE ACCOUNTING: synctest.Test fails if a
// goroutine started inside the bubble is still running when the test body
// returns. That turns "did the janitor actually exit?" — normally an awkward
// thing to assert — into something the framework checks for us.

// TestRateLimitJanitorEvictsStaleClients checks the sweep actually happens.
func TestRateLimitJanitorEvictsStaleClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := &application{
			logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			shutdown: make(chan struct{}),
		}

		app.config.limiter.enabled = true
		// ── Choosing these numbers carefully ─────────────────────────────────
		//
		// A very slow refill is what makes this test meaningful. With burst 1
		// and 0.001 requests/second, a client gets one request and then waits
		// ~16 minutes for the next token.
		//
		// So when the same IP is allowed through again after only four minutes,
		// there is exactly one explanation: its entry was evicted and a BRAND
		// NEW limiter — with a full burst — was created for it. With a
		// realistic rate the bucket would have refilled on its own and the test
		// would pass whether or not the janitor did anything.
		app.config.limiter.rps = 0.001
		app.config.limiter.burst = 1

		handler := app.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		const ip = "203.0.113.42"

		// Burn the single token.
		assert.Equal(t, callWithIP(handler, ip), http.StatusOK)
		// Nothing left, and nothing due for a quarter of an hour.
		assert.Equal(t, callWithIP(handler, ip), http.StatusTooManyRequests)

		// Four minutes of fake time: past the janitor's one-minute tick and its
		// three-minute staleness threshold, but nowhere near a refill.
		time.Sleep(4 * time.Minute)

		// synctest.Wait blocks until every other goroutine in the bubble is
		// durably blocked — i.e. the janitor has finished its sweep and gone
		// back to waiting on the ticker. Without it we could look at the map
		// mid-sweep.
		synctest.Wait()

		if got := callWithIP(handler, ip); got != http.StatusOK {
			t.Errorf("got %d after 4 minutes idle; want 200 — the janitor should have evicted the stale client and given it a fresh limiter", got)
		}

		// Let the janitor exit, or the bubble reports it as leaked.
		app.stop()
		synctest.Wait()
	})
}

// TestRateLimitJanitorExitsOnShutdown is the direct test of the exit path.
//
// The assertion is invisible: if the janitor ignored app.shutdown and kept
// looping, it would still be running when this function returns, and
// synctest.Test would fail the test with a leaked-goroutine panic. Restore the
// book's `for { time.Sleep(time.Minute) }` and this goes red.
//
// That's also why the janitor needed an exit path at all — without one, no test
// in this package could use a bubble, because every call to routes() would
// leave a goroutine behind.
func TestRateLimitJanitorExitsOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := &application{
			logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			shutdown: make(chan struct{}),
		}
		app.config.limiter.enabled = true
		app.config.limiter.rps = 100
		app.config.limiter.burst = 100

		handler := app.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// Put a client in the map so the janitor has something to look at.
		assert.Equal(t, callWithIP(handler, "203.0.113.1"), http.StatusOK)

		// Let it complete a couple of sweeps first, so we're testing that a
		// RUNNING loop stops — not that it never started.
		time.Sleep(2 * time.Minute)
		synctest.Wait()

		app.stop()

		// If the janitor is well-behaved it returns here. If not, synctest
		// fails the test when this function returns.
		synctest.Wait()
	})
}

// TestApplicationStopIsIdempotent checks that stop() can be called twice.
//
// It's easy to dismiss this as trivial, but closing an already-closed channel
// PANICS in Go, and there are two callers that can both reasonably fire: serve()
// on its shutdown path, and a test's t.Cleanup. Without the sync.Once guarding
// it, that combination would take the process down during shutdown — the worst
// possible moment.
func TestApplicationStopIsIdempotent(t *testing.T) {
	app := &application{shutdown: make(chan struct{})}

	app.stop()
	app.stop()
	app.stop()

	select {
	case <-app.shutdown:
		// Closed, as expected.
	default:
		t.Error("shutdown channel was not closed by stop()")
	}

	// And an application that never had the channel initialised — every
	// construction in the older tests, and anything a reader writes by hand —
	// must not panic either.
	(&application{}).stop()
}

// callWithIP runs one request through a handler as the given client IP and
// returns the status.
//
// This drives the handler directly rather than over a socket, which is what
// makes it usable inside a synctest bubble: a real network connection would
// involve goroutines outside the bubble, and the fake clock would never advance.
func callWithIP(h http.Handler, ip string) int {
	r := httptest.NewRequest(http.MethodGet, "/v1/healthcheck", nil)
	r.Header.Set("X-Forwarded-For", ip)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	return rr.Code
}

// getAsClient issues a GET presenting itself as the given client IP.
//
// The testServer.get helper can't do this because it doesn't expose the
// request, and every request in a test otherwise comes from 127.0.0.1 — which
// makes per-client behaviour impossible to observe.
//
// Worth noting what this demonstrates: X-Forwarded-For is client-supplied and
// trivially spoofed, exactly as rateLimit's comment warns. Being able to dodge
// the limiter from a test by setting a header is the same thing an attacker
// would do if the API were exposed without a proxy that overwrites it.
func getAsClient(t *testing.T, ts *testServer, urlPath, ip string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, ts.URL+urlPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("X-Forwarded-For", ip)

	rs, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Body.Close()

	io.Copy(io.Discard, rs.Body)

	return rs.StatusCode
}
