package mailer

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"greenlight/internal/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// TESTING EMAIL WITHOUT AN SMTP DEPENDENCY
// ─────────────────────────────────────────────────────────────────────────────
//
// The obvious objection to testing this package is "you'd need a mail server".
// You do — but SMTP's happy path is about a dozen lines of line-oriented text,
// so a fake one fits in the bottom of this file with no new dependency.
//
// That buys something a template-rendering-only test can't: these tests assert
// on the bytes that actually go out on the wire, through the real go-mail
// dialer, including the MIME structure. If the multipart assembly broke, or
// AddAlternative stopped being called after SetBody, a rendering test would
// still pass and this one wouldn't.
//
// cmd/api mocks the Mailer interface instead — that's the right call there,
// because those tests are about handlers. This is the one place the real
// implementation gets exercised.

// TestSendDeliversRenderedEmail is the end-to-end happy path.
func TestSendDeliversRenderedEmail(t *testing.T) {
	srv := newFakeSMTP(t)

	m := New(srv.host, srv.port, "", "", "Greenlight <no-reply@greenlight.example>")

	data := map[string]any{
		"userID":          int64(42),
		"activationToken": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"name":            "Alice",
	}

	assert.NilError(t, m.Send("alice@example.com", "user_welcome.tmpl", data))

	msgs := srv.messages()
	assert.Equal(t, len(msgs), 1)

	got := msgs[0]

	// The SMTP envelope — what the server was told, independent of the headers.
	assert.Equal(t, got.from, "no-reply@greenlight.example")
	assert.DeepEqual(t, got.recipients, []string{"alice@example.com"})

	// Headers.
	assert.StringContains(t, got.body, "To: alice@example.com")
	assert.StringContains(t, got.body, "From: Greenlight <no-reply@greenlight.example>")
	assert.StringContains(t, got.body, "Subject: Welcome to Greenlight!")

	// Both alternatives must be present. This is the assertion that would fail
	// if AddAlternative were called before SetBody, which the implementation's
	// comment warns about.
	assert.StringContains(t, got.body, "multipart/alternative")
	assert.StringContains(t, got.body, "text/plain")
	assert.StringContains(t, got.body, "text/html")

	// And the payload that matters: a user who can't find this token can't
	// activate their account.
	assert.StringContains(t, got.body, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	assert.StringContains(t, got.body, "Alice")
	assert.StringContains(t, got.body, "42")
}

// TestSendRendersBothAlternativesFromOneTemplate pins the three-named-template
// contract that Send's doc comment describes.
//
// If someone adds a new template file and forgets one of the three defines,
// Send returns an error rather than sending a half-formed email — and this test
// documents which names are required.
func TestSendRendersAllThreeNamedTemplates(t *testing.T) {
	srv := newFakeSMTP(t)

	m := New(srv.host, srv.port, "", "", "test@example.com")

	err := m.Send("bob@example.com", "user_welcome.tmpl", map[string]any{
		"userID": int64(1), "activationToken": "TOKEN", "name": "Bob",
	})
	assert.NilError(t, err)

	body := srv.messages()[0].body

	// Subject came from {{define "subject"}}.
	assert.StringContains(t, body, "Subject: Welcome to Greenlight!")
	// plainBody's distinctive line.
	assert.StringContains(t, body, "Thanks for signing up for a Greenlight account")
	// htmlBody's distinctive markup — quoted-printable encodes the '=' in
	// tags, so match on something that survives the encoding.
	assert.StringContains(t, body, "doctype html")
}

// TestSendEscapesUserSuppliedData is the security-relevant one.
//
// The user's name comes straight from a registration request, so it's
// attacker-controlled. Send parses templates with html/template (not
// text/template), which escapes on output.
//
// Worth knowing, and pinned here because it surprises people: html/template
// escapes ALL three named templates, including plainBody. So a name containing
// an apostrophe arrives in the plain-text part as &#39;. That's ugly but safe;
// the alternative (text/template for the plain part) would leave the HTML part
// vulnerable if the two ever got mixed up.
func TestSendEscapesUserSuppliedData(t *testing.T) {
	srv := newFakeSMTP(t)

	m := New(srv.host, srv.port, "", "", "test@example.com")

	err := m.Send("mallory@example.com", "user_welcome.tmpl", map[string]any{
		"userID":          int64(7),
		"activationToken": "TOKEN12345",
		"name":            `<script>alert("xss")</script>`,
	})
	assert.NilError(t, err)

	body := srv.messages()[0].body

	// The raw tag must not appear anywhere in the message.
	if strings.Contains(body, "<script>") {
		t.Errorf("unescaped <script> tag reached the wire:\n%s", body)
	}

	// And the escaped form must, proving the name was included rather than
	// silently dropped.
	assert.StringContains(t, body, "&lt;script&gt;")
}

// TestSendUnknownTemplate checks the ParseFS error path.
//
// Note this returns BEFORE any network activity, which is why no fake server is
// needed: a typo'd template name is caught at render time, not send time.
func TestSendUnknownTemplate(t *testing.T) {
	// Port 1 is reserved and nothing listens there — if Send tried to dial,
	// the error would say so and this test would tell us.
	m := New("127.0.0.1", 1, "", "", "test@example.com")

	err := m.Send("alice@example.com", "no_such_template.tmpl", nil)
	if err == nil {
		t.Fatal("got nil error for a non-existent template; want a parse failure")
	}

	assert.StringContains(t, err.Error(), "no_such_template.tmpl")
}

// TestSendTemplateExecutionError covers the ExecuteTemplate error path — data
// that doesn't fit the template.
//
// The templates index into a map, so a nil map renders "<no value>" rather than
// erroring. Passing a value that isn't indexable at all is what fails.
func TestSendTemplateExecutionError(t *testing.T) {
	m := New("127.0.0.1", 1, "", "", "test@example.com")

	// A string can't be indexed by ".name", so plainBody fails to execute.
	err := m.Send("alice@example.com", "user_welcome.tmpl", "not a map")
	if err == nil {
		t.Fatal("got nil error executing a template against an unindexable value; want a failure")
	}
}

// TestSendRetriesBeforeGivingUp covers the retry loop.
//
// synctest gives the bubble a fake clock, so the two 500ms sleeps between the
// three attempts cost nothing. Without it this test would take a real second —
// and the whole point of a retry loop is that you can't verify it without
// either waiting or faking time.
func TestSendRetriesBeforeGivingUp(t *testing.T) {
	// A listener that accepts connections and immediately hangs up, so every
	// attempt fails at the SMTP greeting rather than at the TCP layer. Started
	// outside the bubble deliberately: its goroutines are not the test's.
	srv := newRejectingSMTP(t)

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()

		m := New(srv.host, srv.port, "", "", "test@example.com")

		err := m.Send("alice@example.com", "user_welcome.tmpl", map[string]any{
			"userID": int64(1), "activationToken": "TOKEN", "name": "Alice",
		})
		if err == nil {
			t.Fatal("got nil error sending to a server that rejects everything; want a failure")
		}

		// Three attempts, with a sleep after each — including the last, which
		// is a small wart in the implementation but is the current behaviour.
		elapsed := time.Since(start)
		if elapsed < 1500*time.Millisecond {
			t.Errorf("Send returned after %v of fake time; want >= 1.5s, i.e. 3 attempts with 500ms between them", elapsed)
		}

		// At least 3 connections were attempted.
		if got := srv.connections(); got < 3 {
			t.Errorf("got %d connection attempts; want at least 3", got)
		}
	})
}

// TestNewConfiguresDialer pins the settings New applies.
//
// The timeout is the one that matters: without it an unresponsive mail server
// would hang the background goroutine that called Send forever, and since
// graceful shutdown waits on those goroutines, the server would never exit.
func TestNewConfiguresDialer(t *testing.T) {
	m := New("smtp.example.com", 25, "user", "pass", "Greenlight <no-reply@example.com>")

	assert.Equal(t, m.sender, "Greenlight <no-reply@example.com>")
	assert.Equal(t, m.dialer.Timeout, 5*time.Second)
	assert.Equal(t, m.dialer.Host, "smtp.example.com")
	assert.Equal(t, m.dialer.Port, 25)
	assert.Equal(t, m.dialer.Username, "user")
}

// ─────────────────────────────────────────────────────────────────────────────
// A MINIMAL FAKE SMTP SERVER
// ─────────────────────────────────────────────────────────────────────────────
//
// Enough of RFC 5321 to satisfy net/smtp, which is what go-mail uses
// underneath. Deliberately does NOT advertise STARTTLS or AUTH: go-mail's
// dialer only attempts them when the server offers them, so leaving them out
// keeps the conversation to plain text.

type receivedMessage struct {
	from       string
	recipients []string
	body       string
}

type fakeSMTP struct {
	host string
	port int

	mu       sync.Mutex
	received []receivedMessage
	conns    int
}

func (s *fakeSMTP) messages() []receivedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]receivedMessage(nil), s.received...)
}

func (s *fakeSMTP) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.conns
}

// newFakeSMTP starts a server that speaks just enough SMTP to accept mail.
func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	return startSMTP(t, true)
}

// newRejectingSMTP starts a server that accepts the TCP connection and then
// immediately closes it, so every send attempt fails.
func newRejectingSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	return startSMTP(t, false)
}

func startSMTP(t *testing.T, accept bool) *fakeSMTP {
	t.Helper()

	// Port 0 asks the OS for any free port, so parallel tests never collide.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting fake SMTP listener: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	addr := ln.Addr().(*net.TCPAddr)
	srv := &fakeSMTP{host: addr.IP.String(), port: addr.Port}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// The listener was closed by cleanup; we're done.
				return
			}

			srv.mu.Lock()
			srv.conns++
			srv.mu.Unlock()

			if !accept {
				conn.Close()
				continue
			}

			go srv.handle(conn)
		}
	}()

	return srv
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()

	// A generous deadline so a stuck test fails rather than hanging forever.
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	var (
		reader = bufio.NewReader(conn)
		msg    receivedMessage
	)

	fmt.Fprint(conn, "220 fake.smtp.test ESMTP ready\r\n")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")
		verb, rest, _ := strings.Cut(line, " ")

		switch strings.ToUpper(verb) {
		case "EHLO":
			// Multi-line reply: every line but the last uses '-' after the
			// code. Note the deliberate absence of STARTTLS and AUTH.
			fmt.Fprint(conn, "250-fake.smtp.test\r\n")
			fmt.Fprint(conn, "250-8BITMIME\r\n")
			fmt.Fprint(conn, "250 SIZE 35882577\r\n")

		case "HELO":
			fmt.Fprint(conn, "250 fake.smtp.test\r\n")

		case "MAIL":
			msg.from = extractAddress(rest)
			fmt.Fprint(conn, "250 2.1.0 OK\r\n")

		case "RCPT":
			msg.recipients = append(msg.recipients, extractAddress(rest))
			fmt.Fprint(conn, "250 2.1.5 OK\r\n")

		case "DATA":
			fmt.Fprint(conn, "354 Start mail input; end with <CRLF>.<CRLF>\r\n")

			var body strings.Builder

			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}

				// A lone "." terminates the message body.
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}

				body.WriteString(dataLine)
			}

			msg.body = body.String()

			s.mu.Lock()
			s.received = append(s.received, msg)
			s.mu.Unlock()

			msg = receivedMessage{}

			fmt.Fprint(conn, "250 2.0.0 OK\r\n")

		case "RSET":
			msg = receivedMessage{}
			fmt.Fprint(conn, "250 2.0.0 OK\r\n")

		case "NOOP":
			fmt.Fprint(conn, "250 2.0.0 OK\r\n")

		case "QUIT":
			fmt.Fprint(conn, "221 2.0.0 Bye\r\n")
			return

		default:
			fmt.Fprint(conn, "500 5.5.1 Unrecognised command\r\n")
		}
	}
}

// extractAddress pulls the address out of "FROM:<a@b.com> BODY=8BITMIME".
func extractAddress(s string) string {
	open := strings.Index(s, "<")
	close := strings.Index(s, ">")

	if open == -1 || close == -1 || close < open {
		return strings.TrimSpace(s)
	}

	return s[open+1 : close]
}
