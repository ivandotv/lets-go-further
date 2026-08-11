package main

import (
	"errors"
	"expvar"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/tomasen/realip"
	"golang.org/x/time/rate"

	"greenlight/internal/data"
	"greenlight/internal/validator"
)

// Every middleware here has the same shape:
//
//	func (app *application) name(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        // ...do something before...
//	        next.ServeHTTP(w, r)
//	        // ...do something after...
//	    })
//	}
//
// That signature — take a Handler, return a Handler — is what makes them
// composable. See routes.go for how they're chained together.

// recoverPanic converts a panic in a handler into a clean 500 response.
//
// Without it, a panic unwinds the goroutine and Go's http.Server logs the stack
// trace and closes the connection abruptly — the client sees an empty reply
// with no status code at all.
//
// IMPORTANT LIMITATION: this only recovers panics in the goroutine handling
// THIS request. A panic in a goroutine you spawn will still crash the whole
// process, which is exactly why app.background() has its own recover.
func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deferred functions run as the stack unwinds during a panic, which is
		// the only place recover() does anything.
		defer func() {
			if err := recover(); err != nil {
				// Tell Go's server to close this connection once the response
				// is sent. The connection's state is unknown after a panic, so
				// reusing it for keep-alive would be risky.
				w.Header().Set("Connection", "close")

				// recover() returns `any`, so we normalise it into an error.
				// %v handles whatever was panicked with — string, error, or
				// anything else.
				app.serverErrorResponse(w, r, fmt.Errorf("%v", err))
			}
		}()

		next.ServeHTTP(w, r)
	})
}

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

// authenticate identifies the user making the request, if any.
//
// It runs on EVERY request and always sets a user in the context — either a
// real one or data.AnonymousUser. Downstream middleware and handlers can then
// rely on contextGetUser never failing.
//
// Note that this middleware doesn't reject anyone; it only identifies. Enforcing
// access is the job of requireAuthenticatedUser and friends below. Splitting
// "who are you" from "are you allowed" keeps each piece simple.
func (app *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tell caches that the response varies by Authorization header, so a
		// proxy can't serve one user's response to another.
		w.Header().Add("Vary", "Authorization")

		authorizationHeader := r.Header.Get("Authorization")

		// No header at all → anonymous. This is not an error: plenty of
		// endpoints are public.
		if authorizationHeader == "" {
			r = app.contextSetUser(r, data.AnonymousUser)
			next.ServeHTTP(w, r)
			return
		}

		// Expected format is exactly: "Bearer <token>"
		headerParts := strings.Split(authorizationHeader, " ")
		if len(headerParts) != 2 || headerParts[0] != "Bearer" {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		token := headerParts[1]

		// Cheap shape check before touching the database.
		v := validator.New()
		if data.ValidateTokenPlaintext(v, token); !v.Valid() {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		user, err := app.models.Users.GetForToken(data.ScopeAuthentication, token)
		if err != nil {
			switch {
			case errors.Is(err, data.ErrRecordNotFound):
				app.invalidAuthenticationTokenResponse(w, r)
			default:
				app.serverErrorResponse(w, r, err)
			}
			return
		}

		r = app.contextSetUser(r, user)

		next.ServeHTTP(w, r)
	})
}

// requireAuthenticatedUser rejects anonymous requests with a 401.
//
// Note the signature: it takes and returns http.HandlerFunc rather than
// http.Handler. That's so these can be composed directly around a handler
// function in routes.go without extra conversions.
func (app *application) requireAuthenticatedUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		if user.IsAnonymous() {
			app.authenticationRequiredResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}
}

// requireActivatedUser rejects users who haven't activated their account.
//
// It WRAPS requireAuthenticatedUser rather than repeating the anonymous check,
// so "activated" automatically implies "authenticated". Composing the checks
// like this makes it impossible to accidentally allow an anonymous user
// through by forgetting a wrapper in routes.go.
func (app *application) requireActivatedUser(next http.HandlerFunc) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		if !user.Activated {
			app.inactiveAccountResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}

	return app.requireAuthenticatedUser(fn)
}

// requirePermission checks that the user holds a specific permission code.
//
// Like the above, it composes: permission implies activated implies
// authenticated.
func (app *application) requirePermission(code string, next http.HandlerFunc) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		permissions, err := app.models.Permissions.GetAllForUser(user.ID)
		if err != nil {
			app.serverErrorResponse(w, r, err)
			return
		}

		if !permissions.Include(code) {
			app.notPermittedResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}

	return app.requireActivatedUser(fn)
}

// enableCORS implements Cross-Origin Resource Sharing for a safelist of
// trusted origins.
//
// Browsers block cross-origin JavaScript requests unless the server explicitly
// opts in with an Access-Control-Allow-Origin header. This middleware sends
// that header — but only for origins we've been configured to trust.
//
// We never send `Access-Control-Allow-Origin: *` because that's incompatible
// with credentialed requests and would let any site on the internet call the
// API from a victim's browser.
func (app *application) enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both Vary headers are essential for cache correctness. Without them
		// a shared cache could store the response for origin A and serve it to
		// origin B, defeating the safelist entirely.
		w.Header().Add("Vary", "Origin")
		w.Header().Add("Vary", "Access-Control-Request-Method")

		origin := r.Header.Get("Origin")

		if origin != "" {
			for _, trusted := range app.config.cors.trustedOrigins {
				if origin != trusted {
					continue
				}

				w.Header().Set("Access-Control-Allow-Origin", origin)

				// ── Preflight requests ───────────────────────────────────────
				//
				// Before sending a "non-simple" request (anything using PUT,
				// PATCH, DELETE, or a Content-Type of application/json), a
				// browser first sends an OPTIONS request asking permission.
				//
				// We identify it by the presence of the
				// Access-Control-Request-Method header — an OPTIONS request
				// without it is a regular request, not a preflight.
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					w.Header().Set("Access-Control-Allow-Methods", "OPTIONS, PUT, PATCH, DELETE")
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

					// Cache the preflight result for 60 seconds so the browser
					// doesn't repeat it before every request.
					w.Header().Set("Access-Control-Max-Age", "60")

					// 200, not 204: some legacy browsers choke on a 204 here.
					// We return immediately — a preflight must not reach the
					// actual handler.
					w.WriteHeader(http.StatusOK)
					return
				}

				break
			}
		}

		next.ServeHTTP(w, r)
	})
}

// secureHeaders sets a handful of defensive HTTP headers on every response.
//
// This one comes from the FIRST book ("Let's Go", chapter 6). It's aimed
// mostly at browsers, so it matters for a JSON API served to a web front end —
// particularly if anyone ever navigates directly to an endpoint.
func (app *application) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stop browsers MIME-sniffing a response into something executable.
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Disallow framing, to prevent clickjacking.
		w.Header().Set("X-Frame-Options", "deny")

		// Don't leak our URLs (which may contain IDs) to third-party sites in
		// the Referer header.
		w.Header().Set("Referrer-Policy", "origin-when-cross-origin")

		// A restrictive CSP: this API returns JSON, so nothing should ever be
		// loaded or executed from it.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

		next.ServeHTTP(w, r)
	})
}

// logRequest logs every request once it has completed.
//
// Also from the first book. Logging AFTER the handler runs (rather than before)
// lets us include the response status and duration, which is what you actually
// want when debugging.
func (app *application) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httpsnoop wraps the ResponseWriter to capture what the handler wrote.
		//
		// Writing our own wrapper is deceptively hard: http.ResponseWriter has
		// optional interfaces (Flusher, Hijacker, Pusher) and a naive wrapper
		// silently breaks them. httpsnoop preserves whichever ones the
		// original implemented.
		metrics := httpsnoop.CaptureMetrics(next, w, r)

		app.logger.Info("request",
			"ip", realip.FromRequest(r),
			"proto", r.Proto,
			"method", r.Method,
			"uri", r.URL.RequestURI(),
			"status", metrics.Code,
			"bytes", metrics.Written,
			"duration", metrics.Duration.String(),
		)
	})
}

// The expvar counters used by the metrics middleware below.
//
// ── WHY THESE ARE PACKAGE-LEVEL ──────────────────────────────────────────────
//
// expvar keeps a single process-wide registry, and expvar.NewInt PANICS if the
// name is already taken. The book declares these inside the metrics function,
// which is fine when routes() is only ever called once — but the test suite
// builds a fresh application (and therefore a fresh middleware chain) for every
// test, and the second call would blow up with:
//
//	panic: Reuse of exported var name: total_requests_received
//
// Declaring them at package level means they're registered exactly once, when
// the package is initialised, no matter how many chains get built.
//
// expvar's Int and Map types are safe for concurrent use, so no mutex is
// needed despite every request touching them.
var (
	totalRequestsReceived           = expvar.NewInt("total_requests_received")
	totalResponsesSent              = expvar.NewInt("total_responses_sent")
	totalProcessingTimeMicroseconds = expvar.NewInt("total_processing_time_μs")
	totalResponsesSentByStatus      = expvar.NewMap("total_responses_sent_by_status")
)

// metrics records request counts, response times and status codes, publishing
// them via expvar at GET /debug/vars.
func (app *application) metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalRequestsReceived.Add(1)

		m := httpsnoop.CaptureMetrics(next, w, r)

		totalResponsesSent.Add(1)
		totalProcessingTimeMicroseconds.Add(m.Duration.Microseconds())

		// Dividing total_processing_time_μs by total_responses_sent gives the
		// mean response time. (A mean hides tail latency — for real production
		// monitoring you'd want percentiles from something like Prometheus —
		// but it's a useful signal and it's free.)

		// expvar.Map keys are strings, so convert the status code.
		totalResponsesSentByStatus.Add(strconv.Itoa(m.Code), 1)
	})
}
