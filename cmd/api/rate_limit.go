package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/tomasen/realip"
	"golang.org/x/time/rate"
)

// rateLimit applies a per-client token-bucket rate limit.
//
// ── HOW THE TOKEN BUCKET WORKS ───────────────────────────────────────────────
//
// Each client gets a bucket that holds up to `burst` tokens and refills at
// `rps` tokens per second. Each request removes one token; if the bucket is
// empty, the request is rejected with a 429.
//
// This allows short bursts of activity (up to `burst` requests back to back)
// while capping the sustained rate — which matches how real clients behave far
// better than a hard "N per second" cap would.
func (app *application) rateLimit(next http.Handler) http.Handler {
	// client bundles a limiter with the last time we saw the client, so we can
	// clean up stale entries.
	type client struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}

	// These are declared OUTSIDE the returned closure, so they're initialised
	// once when routes() builds the chain — not on every request. The closure
	// captures them.
	var (
		// A mutex is required because http.Server handles each request in its
		// own goroutine, and Go maps are NOT safe for concurrent use. Without
		// this you'd get "concurrent map writes" and a hard crash under load.
		mu      sync.Mutex
		clients = make(map[string]*client)
	)

	// Background janitor: without it, `clients` grows forever — one entry per
	// IP that ever connected. That's an unbounded memory leak, and on a public
	// API it's a trivial way to exhaust the server's memory.
	//
	// ── WHY THIS LOOP HAS AN EXIT ────────────────────────────────────────────
	//
	// The book writes this as `for { time.Sleep(time.Minute); ... }`, with no
	// way out. In production that's nearly harmless: routes() is called once, so
	// it's a single goroutine that lives as long as the process.
	//
	// It's still worth closing, for two reasons. It's a goroutine that outlives
	// the graceful shutdown that serve() works so hard to get right — the
	// process waits for in-flight requests and background email, then exits with
	// this still running. And in a test binary, routes() is called once per
	// test, so the goroutines accumulate; anything that checks for leaked
	// goroutines (testing/synctest, goleak) trips over it immediately, which is
	// what makes the janitor's behaviour hard to test at all.
	//
	// app.shutdown is nil unless someone set it, and a receive on a nil channel
	// blocks forever — so the default behaviour is exactly the book's, and only
	// callers that opt in (run() and the test harness) get the exit.
	go func() {
		// A ticker rather than Sleep, so the sweep interval doesn't drift by
		// however long the sweep itself took.
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				mu.Lock()
				for ip, c := range clients {
					if time.Since(c.lastSeen) > 3*time.Minute {
						delete(clients, ip)
					}
				}
				mu.Unlock()

			case <-app.shutdown:
				return
			}
		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The limiter can be turned off entirely — very handy in tests, where
		// firing dozens of requests would otherwise trip it.
		if !app.config.limiter.enabled {
			next.ServeHTTP(w, r)
			return
		}

		// realip.FromRequest reads X-Forwarded-For / X-Real-IP when present,
		// falling back to the socket address. Behind a load balancer,
		// r.RemoteAddr is the balancer's IP — so without this, every client
		// would share a single bucket.
		//
		// Security note: those headers are client-supplied and trivially
		// spoofed. This is only safe when you are genuinely behind a proxy that
		// overwrites them. Exposed directly to the internet, an attacker can
		// dodge the limiter by rotating the header.
		ip := realip.FromRequest(r)

		mu.Lock()

		if _, found := clients[ip]; !found {
			clients[ip] = &client{
				limiter: rate.NewLimiter(rate.Limit(app.config.limiter.rps), app.config.limiter.burst),
			}
		}

		clients[ip].lastSeen = time.Now()

		// Allow() takes a token if one is available and reports whether it
		// succeeded. It never blocks.
		if !clients[ip].limiter.Allow() {
			// Unlock explicitly rather than with defer — a deferred unlock
			// would hold the lock for the entire downstream handler, which
			// would serialise every request through the whole application.
			mu.Unlock()
			app.rateLimitExceededResponse(w, r)
			return
		}

		mu.Unlock()

		next.ServeHTTP(w, r)
	})
}
