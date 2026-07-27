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
// The interesting part is graceful shutdown: when the process is asked to stop,
// we want to finish serving in-flight requests (and in-flight background work)
// rather than severing connections mid-response.
func (app *application) serve() error {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", app.config.port),
		Handler: app.routes(),

		// Route the server's own error messages (bad TLS handshakes, malformed
		// requests) through our structured logger instead of the standard
		// library's default, which writes unstructured text to stderr.
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
		// WriteTimeout: max time to write the response.
		IdleTimeout:  time.Minute,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// shutdownError carries the result of the shutdown back to this function
	// from the goroutine below. A buffered channel of size 1 means the sender
	// never blocks even if nobody is reading yet.
	shutdownError := make(chan error, 1)

	go func() {
		// Listen for the two signals that conventionally mean "stop":
		//   SIGINT  — Ctrl+C
		//   SIGTERM — what Docker/Kubernetes/systemd send
		//
		// We deliberately do NOT catch SIGKILL, because it cannot be caught.
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

		// Blocks until a signal arrives.
		s := <-quit

		app.logger.Info("shutting down server", "signal", s.String())

		// Give in-flight requests up to 30 seconds to complete.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Shutdown stops accepting new connections, then waits for active
		// requests to finish. It returns nil on a clean shutdown, or the
		// context's error if the deadline passed first.
		if err := srv.Shutdown(ctx); err != nil {
			shutdownError <- err
			return
		}

		// The HTTP requests are done, but background goroutines started via
		// app.background() may still be running (sending a welcome email, say).
		// Wait for those too before declaring shutdown complete.
		app.logger.Info("completing background tasks", "addr", srv.Addr)
		app.wg.Wait()

		shutdownError <- nil
	}()

	app.logger.Info("starting server", "addr", srv.Addr, "env", app.config.env)

	// ListenAndServe blocks until the server stops. When Shutdown is called it
	// returns http.ErrServerClosed immediately — which is the EXPECTED outcome
	// of a graceful shutdown, not a failure. Anything else is a genuine error.
	err := srv.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	// Now block until the shutdown goroutine reports back, so we don't return
	// (and let main exit) before cleanup finishes.
	if err := <-shutdownError; err != nil {
		return err
	}

	app.logger.Info("stopped server", "addr", srv.Addr)

	return nil
}
