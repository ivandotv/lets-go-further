package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"greenlight/internal/data"
	"greenlight/internal/testutil"
)

// ─────────────────────────────────────────────────────────────────────────────
// END-TO-END TEST HARNESS
// ─────────────────────────────────────────────────────────────────────────────
//
// The pattern here is from the first book ("Let's Go", chapter 13): spin up the
// REAL application — real routes, real middleware chain, real database — behind
// an httptest.Server, and drive it over actual HTTP with a real client.
//
// Nothing is mocked except outbound email (which would otherwise need an SMTP
// server). That means these tests exercise routing, middleware ordering,
// authentication, permissions, JSON encoding and SQL all together. If any link
// in that chain breaks, a test here fails — which is exactly what you want from
// an integration test.

// TestMain lowers the bcrypt cost for the whole package. See the equivalent in
// internal/data for the full reasoning.
func TestMain(m *testing.M) {
	data.BcryptCost = bcrypt.MinCost

	os.Exit(m.Run())
}

// mockMailer records what would have been sent instead of sending it.
//
// This is the payoff for declaring `Mailer` as an interface in main.go rather
// than using the concrete type the book uses: tests can inspect the outgoing
// mail, which is the only way to get hold of an activation token.
type mockMailer struct {
	// A mutex because emails are sent from background goroutines, and the test
	// reads them from the main test goroutine. Without it, `go test -race`
	// reports a data race.
	mu      sync.Mutex
	sent    []mockEmail
	sendErr error

	// sendDelay makes Send take a measurable amount of time, so a test can
	// ensure background work is still in flight when something else happens —
	// used by the graceful-shutdown test to prove shutdown waits for it.
	sendDelay time.Duration
}

type mockEmail struct {
	Recipient string
	Template  string
	Data      map[string]any
}

func (m *mockMailer) Send(recipient, templateFile string, data any) error {
	// Read the delay before taking the lock, so a slow "send" doesn't block a
	// test that's only trying to read what's been sent so far.
	m.mu.Lock()
	delay := m.sendDelay
	m.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// The handlers always pass a map[string]any; assert it so tests can read
	// individual keys.
	dataMap, _ := data.(map[string]any)

	m.sent = append(m.sent, mockEmail{
		Recipient: recipient,
		Template:  templateFile,
		Data:      dataMap,
	})

	return m.sendErr
}

// messages returns a copy of everything sent so far, safely.
func (m *mockMailer) messages() []mockEmail {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]mockEmail(nil), m.sent...)
}

// waitForEmail blocks until the application's background work has finished,
// then returns everything sent — failing the test if fewer than n arrived.
//
// This is necessary because registerUserHandler sends mail from a BACKGROUND
// goroutine: the HTTP response comes back before the email is recorded, so
// checking immediately after the request would be a race that passes or fails
// depending on scheduling.
//
// ── WHY wg.Wait() RATHER THAN POLLING ────────────────────────────────────────
//
// This used to poll every 5ms against a 2-second deadline. That works, but it's
// guessing: it's slow when the goroutine is slow, and it can only ever report
// "it didn't happen within 2 seconds" rather than "it didn't happen".
//
// app.background() (main.go) registers every goroutine it starts on app.wg, and
// that's the same WaitGroup graceful shutdown blocks on. So waiting on it here
// is exact — it returns the instant the work is done, and if the work never
// completes the test hangs and the panic dump names the stuck goroutine, which
// is far more useful than a timeout message.
//
// (synctest.Wait() is the other modern option, but it doesn't apply here:
// requests reach the handler over a real socket via httptest.Server, whose
// goroutines are outside any synctest bubble — so the background mail goroutine
// would be invisible to it. synctest is used in this suite where the code under
// test is reached directly; see internal/mailer for an example.)
func (m *mockMailer) waitForEmail(t *testing.T, app *application, n int) []mockEmail {
	t.Helper()

	app.wg.Wait()

	msgs := m.messages()
	if len(msgs) < n {
		t.Fatalf("got %d email(s) after all background work completed; want at least %d", len(msgs), n)
	}

	return msgs
}

// newTestApplication builds an application wired to a fresh database and a
// mock mailer.
func newTestApplication(t *testing.T) (*application, *mockMailer) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping end-to-end test with -short")
	}

	// Discard log output so a passing test run stays readable. Swap
	// io.Discard for os.Stdout when you're debugging a failure — the request
	// logs are very informative.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var cfg config
	cfg.env = "testing"
	// The rate limiter is disabled by default because these tests fire many
	// requests in quick succession from a single IP and would otherwise trip
	// it. TestRateLimit turns it back on deliberately.
	cfg.limiter.enabled = false
	cfg.cors.trustedOrigins = []string{"https://trusted.example.com"}

	mailer := &mockMailer{}

	app := &application{
		config: cfg,
		logger: logger,
		models: data.NewModels(testutil.NewDB(t)),
		mailer: mailer,
		// Without this the rate limiter's janitor goroutine would run until the
		// test BINARY exits, not until this test does — so a full run would
		// accumulate one per test. Harmless in itself, but it makes any
		// goroutine-leak check (testing/synctest, goleak) useless, and it means
		// the janitor's own behaviour can't be tested.
		shutdown: make(chan struct{}),
	}

	// Cleanup runs when the test and all its subtests finish.
	t.Cleanup(app.stop)

	return app, mailer
}

// ─────────────────────────────────────────────────────────────────────────────
// CAPTURING LOG OUTPUT
// ─────────────────────────────────────────────────────────────────────────────
//
// Most tests point the logger at io.Discard (see newTestApplication). The few
// that assert on what was LOGGED — rather than what was sent — need the two
// helpers below.
//
// ── WHY THE BUFFER NEEDS A MUTEX ─────────────────────────────────────────────
//
// logRequest logs AFTER the handler has run, which means the server writes
// that line on its own goroutine at a moment the client has often already
// received the response. A plain bytes.Buffer read from the test goroutine is
// therefore a genuine data race, and `go test -race` fails on it — not a
// theoretical one.
//
// Waiting correctly is the other half: call ts.Close() before reading.
// httptest.Server.Close blocks until every outstanding request has completed,
// which is precisely the guarantee needed, and it's safe to call twice
// alongside the t.Cleanup(ts.Close) that newTestServer registers.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// newCapturingLogger returns a logger that writes JSON to w.
//
// JSON rather than the TextHandler used elsewhere in the suite, so a test can
// unmarshal a line and assert on individual attributes instead of
// substring-matching a formatted string — which would happily pass on an ID
// that appeared in the wrong field.
func newCapturingLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// testServer wraps httptest.Server with helpers that speak JSON.
type testServer struct {
	*httptest.Server
}

// newTestServer starts a real HTTP server running the application's full
// handler chain.
func newTestServer(t *testing.T, h http.Handler) *testServer {
	t.Helper()

	ts := httptest.NewServer(h)

	// A cookie jar isn't needed for a token-based API, but it costs nothing
	// and means the helper works unchanged if you add session cookies later.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	ts.Client().Jar = jar

	// Don't follow redirects automatically — a test should be able to assert
	// on a 301/302 rather than silently ending up somewhere else.
	ts.Client().CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// Shut the server down when the test finishes.
	t.Cleanup(ts.Close)

	return &testServer{ts}
}

// do sends a request and returns the status, headers and body.
//
// `body` may be nil (no body), a string, or any value that will be marshalled
// to JSON. `token` may be empty for an unauthenticated request.
func (ts *testServer) do(t *testing.T, method, urlPath, token string, body any) (int, http.Header, string) {
	t.Helper()

	var bodyReader io.Reader

	switch b := body.(type) {
	case nil:
		bodyReader = nil
	case string:
		// A raw string lets a test send deliberately malformed JSON.
		bodyReader = bytes.NewReader([]byte(b))
	default:
		js, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		bodyReader = bytes.NewReader(js)
	}

	req, err := http.NewRequest(method, ts.URL+urlPath, bodyReader)
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rs, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Body.Close() }()

	responseBody, err := io.ReadAll(rs.Body)
	if err != nil {
		t.Fatal(err)
	}

	return rs.StatusCode, rs.Header, string(responseBody)
}

// Convenience wrappers so the tests below read cleanly.

func (ts *testServer) get(t *testing.T, urlPath, token string) (int, http.Header, string) {
	return ts.do(t, http.MethodGet, urlPath, token, nil)
}

func (ts *testServer) post(t *testing.T, urlPath, token string, body any) (int, http.Header, string) {
	return ts.do(t, http.MethodPost, urlPath, token, body)
}

func (ts *testServer) patch(t *testing.T, urlPath, token string, body any) (int, http.Header, string) {
	return ts.do(t, http.MethodPatch, urlPath, token, body)
}

func (ts *testServer) put(t *testing.T, urlPath, token string, body any) (int, http.Header, string) {
	return ts.do(t, http.MethodPut, urlPath, token, body)
}

func (ts *testServer) delete(t *testing.T, urlPath, token string) (int, http.Header, string) {
	return ts.do(t, http.MethodDelete, urlPath, token, nil)
}

// unmarshal decodes a JSON response body into dst, failing the test on error.
func unmarshal(t *testing.T, body string, dst any) {
	t.Helper()

	if err := json.Unmarshal([]byte(body), dst); err != nil {
		t.Fatalf("could not unmarshal response body %q: %v", body, err)
	}
}

// createActivatedUser registers a user directly through the model layer,
// activates them, grants the given permissions, and returns a valid
// authentication token.
//
// It bypasses the HTTP layer on purpose: most tests want "a user who can make
// authenticated requests" as a PRECONDITION, not as the thing under test.
// Going through the API for setup would make every test depend on the
// registration and activation endpoints working, so one bug there would fail
// the entire suite and obscure the real cause.
func createActivatedUser(t *testing.T, app *application, email string, permissions ...string) string {
	t.Helper()

	user := &data.User{
		Name:      "Test User",
		Email:     email,
		Activated: true,
	}

	if err := user.Password.Set("pa55word1234"); err != nil {
		t.Fatal(err)
	}
	if err := app.models.Users.Insert(user); err != nil {
		t.Fatal(err)
	}
	if len(permissions) > 0 {
		if err := app.models.Permissions.AddForUser(user.ID, permissions...); err != nil {
			t.Fatal(err)
		}
	}

	token, err := app.models.Tokens.New(user.ID, 24*time.Hour, data.ScopeAuthentication)
	if err != nil {
		t.Fatal(err)
	}

	return token.Plaintext
}

// createTestMovie inserts a movie directly via the model layer, for tests that
// need one to already exist.
func createTestMovie(t *testing.T, app *application, title string, year int32, genres ...string) *data.Movie {
	t.Helper()

	if len(genres) == 0 {
		genres = []string{"drama"}
	}

	movie := &data.Movie{
		Title:   title,
		Year:    year,
		Runtime: 102,
		Genres:  data.Genres(genres),
	}

	if err := app.models.Movies.Insert(movie); err != nil {
		t.Fatal(err)
	}

	return movie
}
