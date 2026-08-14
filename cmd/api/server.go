package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// serve starts the HTTP server and blocks until it shuts down.
//
// It's the last thing run() does, so it blocks for the whole lifetime of the
// process — and returning from it is what finally lets run()'s deferred
// database.Close() execute.
//
// The interesting part is graceful shutdown: when the process is asked to stop,
// we want to finish serving in-flight requests (and in-flight background work)
// rather than severing connections mid-response.
//
// Call it once per process. The error path below returns without draining the
// shutdown goroutine, so a second call would be building on a leaked one.
func (app *application) serve() error {
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", app.config.port),

		// Building the chain has a side effect worth knowing about: rateLimit
		// starts its janitor goroutine here, not on the first request. That's
		// the loop app.stop() exists to shut down, further down this function.
		Handler: app.routes(),

		// Route the server's own error messages (bad TLS handshakes, malformed
		// request lines) through our structured logger instead of the standard
		// library's default, which writes unstructured text to stderr and would
		// break JSON log parsing. slog.NewLogLogger adapts an slog handler into
		// the *log.Logger that net/http wants.
		ErrorLog: slog.NewLogLogger(app.logger.Handler(), slog.LevelError),

		// ── Timeouts ─────────────────────────────────────────────────────────
		//
		// The zero value for all of these is "no timeout", which means a single
		// slow or malicious client can hold a connection open forever. Always
		// set them.
		//
		// IdleTimeout:  how long a keep-alive connection may sit unused.
		// ReadTimeout:  max time from accepting the connection to finishing
		//               reading the request body — caps slow-loris attacks.
		// WriteTimeout: max time to write the response. Note it's much shorter
		//               than the 30s shutdown grace period below, so no
		//               in-flight request can actually use the full grace —
		//               this kills it first. The extra headroom down there is
		//               for connection bookkeeping, not for slow handlers.
		IdleTimeout:  time.Minute,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// shutdownError carries the result of the shutdown back to this function
	// from the goroutine below. A buffered channel of size 1 means the sender
	// never blocks even if nobody is reading yet — which matters on the error
	// path, where serve() returns without ever receiving. Unbuffered, the
	// goroutine would block on the send forever.
	shutdownError := make(chan error, 1)

	go func() {
		// Listen for the two signals that conventionally mean "stop":
		//   SIGINT  — Ctrl+C
		//   SIGTERM — what Docker/Kubernetes/systemd send
		//
		// We deliberately do NOT catch SIGKILL, because it cannot be caught.
		//
		// The channel MUST be buffered: on a signal the runtime does a
		// non-blocking send and simply drops it if there's no room.
		//
		// Registering here also removes Go's default behaviour of terminating
		// on these signals — which is the entire point, but has a consequence:
		// a second Ctrl+C during the drain lands in the buffer with nobody
		// reading it, and does nothing. Aborting a wedged shutdown takes
		// SIGKILL. (For "press again to force", add a second <-quit here that
		// calls os.Exit(1).)
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

		// Blocks until a signal arrives. No matching signal.Stop, since the
		// process is on its way out by the time this returns.
		s := <-quit

		app.logger.Info("shutting down server", "signal", s.String())

		// Give in-flight requests up to 30 seconds to complete. This deadline
		// bounds srv.Shutdown ONLY — not the app.wg.Wait() further down.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Shutdown closes the listeners, closes idle keep-alive connections,
		// then waits for the active ones to go idle. It returns nil on a clean
		// shutdown, or the context's error if the deadline passed first.
		//
		// On that error we bail out early, deliberately skipping stop() and
		// wg.Wait(): a shutdown that has already blown its deadline shouldn't
		// then go on to block indefinitely waiting for background email.
		if err := srv.Shutdown(ctx); err != nil {
			shutdownError <- err
			return
		}

		// Tell the long-lived background loops to stop. These aren't tracked by
		// app.wg — they never "finish", so waiting on them would hang — so they
		// get a signal instead. Right now that's the rate limiter's janitor.
		//
		// This happens AFTER srv.Shutdown returns, so nothing is still serving
		// requests that might depend on them.
		app.stop()

		// The HTTP requests are done, but background goroutines started via
		// app.background() may still be running (sending a welcome email, say).
		// Wait for those too before declaring shutdown complete.
		//
		// wg and shutdown cover opposite cases, and mixing them up is the
		// classic bug: wg means "wait for this to FINISH", shutdown means "tell
		// this to STOP". Waiting on the janitor would hang forever; signalling
		// a half-sent email would lose it.
		//
		// Note this wait has no deadline of its own — a wedged SMTP send would
		// hang the process here. What keeps that honest is internal/mailer's
		// own retry and timeout budget.
		app.logger.Info("completing background tasks", "addr", srv.Addr)
		app.wg.Wait()

		shutdownError <- nil
	}()

	app.logger.Info("starting server", "addr", srv.Addr, "env", app.config.env)

	// ListenAndServe blocks until the server stops. When Shutdown is called it
	// returns http.ErrServerClosed IMMEDIATELY — before the drain has actually
	// finished — so that's the EXPECTED outcome of a graceful shutdown, not a
	// failure. Anything else (port already in use, permission denied) is a
	// genuine error.
	err := srv.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		// Returning here leaves the goroutine above blocked on <-quit forever.
		// Harmless, because the process is exiting anyway — but it's the reason
		// serve() is a once-per-process call.
		return err
	}

	// Now block until the shutdown goroutine reports back, so we don't return
	// (and let main exit) before cleanup finishes.
	//
	// This receive is what actually makes the shutdown graceful. Without it
	// serve() would return the moment Shutdown was CALLED, run() would close
	// the database, and the process would exit with requests and emails still
	// in flight. TestServeGracefulShutdown asserts exactly that.
	if err := <-shutdownError; err != nil {
		return err
	}

	app.logger.Info("stopped server", "addr", srv.Addr)

	return nil
}
