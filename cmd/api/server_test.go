package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"greenlight/internal/assert"
	"greenlight/internal/data"
	"greenlight/internal/testutil"
)

// ─────────────────────────────────────────────────────────────────────────────
// GRACEFUL SHUTDOWN
// ─────────────────────────────────────────────────────────────────────────────
//
// Every other test in this package drives the handler chain through
// httptest.Server, which never exercises serve() at all — so the shutdown
// sequence, which is the most interesting thing in server.go, was entirely
// uncovered.
//
// Testing it means using real signals, because that IS the interface: serve()
// blocks until SIGINT or SIGTERM arrives. Two details make that safe to do
// inside a test binary:
//
//  1. The test registers its own signal.Notify for SIGTERM BEFORE starting the
//     server. Registering a handler anywhere in the process disables the
//     default "terminate immediately" action process-wide, which closes the
//     race where the signal could arrive before serve() has set up its own
//     handler and kill the test run.
//
//  2. The port is chosen by binding :0, reading back what the OS assigned, and
//     releasing it — so parallel runs on a busy machine don't collide.

// TestServeGracefulShutdown checks the whole sequence: in-flight requests
// finish, background work finishes, and serve() returns nil.
func TestServeGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test with -short")
	}

	// See (1) above. Stop it again on the way out so this doesn't affect any
	// test that runs later.
	sink := make(chan os.Signal, 1)
	signal.Notify(sink, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sink) })

	port := freePort(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var cfg config
	cfg.port = port
	cfg.env = "testing"
	cfg.limiter.enabled = false

	mailer := &mockMailer{}

	app := &application{
		config:   cfg,
		logger:   logger,
		models:   data.NewModels(testutil.NewDB(t)),
		mailer:   mailer,
		shutdown: make(chan struct{}),
	}
	t.Cleanup(app.stop)

	serveErr := make(chan error, 1)

	go func() {
		serveErr <- app.serve()
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	waitUntilServing(t, baseURL+"/v1/healthcheck")

	// ── Queue background work the way production does ────────────────────────
	//
	// Registering a user makes registerUserHandler call app.background() to
	// send the welcome email, and the mailer is slowed down so that work is
	// still in flight when the shutdown signal lands.
	//
	// It's tempting to just call app.background() from the test goroutine
	// instead. Don't: `go test -race` correctly reports a race if you do,
	// because nothing orders the test's wg.Add(1) against the shutdown
	// goroutine's wg.Wait() — signal delivery is not a happens-before edge the
	// race detector can see. Going through a real request restores the ordering
	// that exists in production, where srv.Shutdown() waits for the handler
	// that queued the work before wg.Wait() is reached.
	mailer.sendDelay = 250 * time.Millisecond

	registerUser(t, baseURL, "shutdown@example.com")

	// Ask the server to stop, while the welcome email is still sending.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}

	select {
	case err := <-serveErr:
		// http.ErrServerClosed is swallowed by serve() as the expected outcome,
		// so a clean shutdown reports nil.
		assert.NilError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve() did not return within 10s of SIGTERM")
	}

	// The whole point of the app.wg.Wait() in serve(): the process must not
	// exit while a welcome email is halfway sent.
	msgs := mailer.messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d email(s) after shutdown; want 1 — serve() returned before background work finished, so app.wg.Wait() is not being honoured", len(msgs))
	}
	assert.Equal(t, msgs[0].Recipient, "shutdown@example.com")

	// And the listener really is closed.
	if _, err := http.Get(baseURL + "/v1/healthcheck"); err == nil {
		t.Error("server still accepting connections after shutdown")
	}
}

// TestServeReturnsErrorOnUnavailablePort checks the non-graceful path: if the
// port can't be bound, serve() must report it rather than hanging.
//
// This is what turns "the container restarted and nothing happened" into a log
// line saying the address is already in use.
func TestServeReturnsErrorOnUnavailablePort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test with -short")
	}

	// Hold a port open for the duration of the test so the bind fails.
	//
	// This listens on ALL interfaces rather than just loopback, because that's
	// what serve() does (it builds its address as ":port"). Holding only
	// 127.0.0.1:port doesn't necessarily conflict with a later bind of
	// 0.0.0.0:port, and the test would then hang instead of failing.
	ln, err := net.Listen("tcp", ":0")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var cfg config
	cfg.port = ln.Addr().(*net.TCPAddr).Port
	cfg.env = "testing"

	app := &application{
		config:   cfg,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		models:   data.NewModels(testutil.NewDB(t)),
		mailer:   &mockMailer{},
		shutdown: make(chan struct{}),
	}
	// serve() returns early on a bind failure without reaching its shutdown
	// path, so the janitor it started needs stopping here.
	t.Cleanup(app.stop)

	done := make(chan error, 1)
	go func() { done <- app.serve() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got nil error binding an already-used port; want a failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() hung instead of reporting a bind failure")
	}
}

// registerUser drives a real registration request, which is what makes the
// server start background work.
func registerUser(t *testing.T, baseURL, email string) {
	t.Helper()

	body := fmt.Sprintf(`{"name":"Shutdown Tester","email":%q,"password":"pa55word1234"}`, email)

	rs, err := http.Post(baseURL+"/v1/users", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("registering a user: %v", err)
	}
	defer func() { _ = rs.Body.Close() }()

	_, _ = io.Copy(io.Discard, rs.Body)

	if rs.StatusCode != http.StatusAccepted {
		t.Fatalf("registering a user: got status %d; want 202", rs.StatusCode)
	}
}

// freePort asks the OS for an unused port and hands it back.
//
// There's an unavoidable gap between releasing the port and the server binding
// it, but on a test machine that window is microseconds and nothing else is
// racing for ephemeral ports.
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer func() { _ = ln.Close() }()

	return ln.Addr().(*net.TCPAddr).Port
}

// waitUntilServing polls until the server answers, so the test doesn't race
// against ListenAndServe starting up.
func waitUntilServing(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		rs, err := http.Get(url)
		if err == nil {
			_ = rs.Body.Close()
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("server at %s did not start within 5s", url)
}
