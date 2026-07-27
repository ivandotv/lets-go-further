// Package mailer sends templated emails over SMTP.
package mailer

import (
	"bytes"
	"embed"
	"html/template"
	"time"

	mail "github.com/go-mail/mail/v2"
)

// templateFS holds the email templates, compiled into the binary.
//
// Same reasoning as the migrations: the binary should be self-contained, and
// nobody should be able to break email by moving a directory.
//
//go:embed "templates"
var templateFS embed.FS

// Mailer wraps a go-mail dialer plus the sender address we send from.
type Mailer struct {
	dialer *mail.Dialer
	sender string
}

// New returns a Mailer configured for the given SMTP server.
//
// sender should be in the form "Greenlight <no-reply@example.com>".
func New(host string, port int, username, password, sender string) *Mailer {
	dialer := mail.NewDialer(host, port, username, password)

	// A 5-second timeout on the whole SMTP conversation. Without this, an
	// unresponsive mail server would hang the sending goroutine indefinitely.
	dialer.Timeout = 5 * time.Second

	return &Mailer{
		dialer: dialer,
		sender: sender,
	}
}

// Send renders templateFile with data and emails the result to recipient.
//
// The template file must define three named templates:
//
//	{{define "subject"}}   ...the subject line
//	{{define "plainBody"}} ...the text/plain part
//	{{define "htmlBody"}}  ...the text/html part
//
// Sending both a plain-text and an HTML body is standard practice: mail
// clients that can't (or won't) render HTML fall back to the plain part.
func (m *Mailer) Send(recipient, templateFile string, data any) error {
	// ParseFS parses the template out of the embedded filesystem.
	tmpl, err := template.New("email").ParseFS(templateFS, "templates/"+templateFile)
	if err != nil {
		return err
	}

	// Execute each named template into a buffer. We use a buffer rather than
	// writing directly anywhere, because a template can fail *partway
	// through* execution — and if we'd already started writing to the real
	// destination we'd have emitted a half-rendered email.
	subject := new(bytes.Buffer)
	if err := tmpl.ExecuteTemplate(subject, "subject", data); err != nil {
		return err
	}

	plainBody := new(bytes.Buffer)
	if err := tmpl.ExecuteTemplate(plainBody, "plainBody", data); err != nil {
		return err
	}

	htmlBody := new(bytes.Buffer)
	if err := tmpl.ExecuteTemplate(htmlBody, "htmlBody", data); err != nil {
		return err
	}

	msg := mail.NewMessage()
	msg.SetHeader("To", recipient)
	msg.SetHeader("From", m.sender)
	msg.SetHeader("Subject", subject.String())
	msg.SetBody("text/plain", plainBody.String())
	// AddAlternative must be called AFTER SetBody. go-mail writes the parts in
	// the order they were added, and the MIME spec says the richest
	// alternative goes last.
	msg.AddAlternative("text/html", htmlBody.String())

	// Retry a few times before giving up. Transient SMTP failures (a brief
	// network blip, a rate-limited relay) are common enough that one attempt
	// is not enough, and mail is not latency-critical.
	var lastErr error
	for i := 1; i <= 3; i++ {
		lastErr = m.dialer.DialAndSend(msg)
		if lastErr == nil {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}

	return lastErr
}
