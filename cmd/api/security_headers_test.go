package main

import (
	"testing"

	"greenlight/internal/assert"
)

// TestSecureHeaders checks that every response carries the browser-hardening
// headers set by the secureHeaders middleware.
//
// These are cheap to set and easy to lose — one reordered middleware in
// routes.go and they silently stop appearing, with nothing else failing. A
// browser won't complain about a MISSING security header; it just quietly stops
// enforcing the thing the header asked for. That silence is the whole reason
// this test exists.
//
// It's reasonable to ask why a JSON API needs them at all, given a browser
// isn't the intended client. The answer is that a browser can still be POINTED
// at these URLs — by a phishing link, an <iframe>, or an <img> tag — and the
// headers are what limit the damage when that happens.
//
// Note the single request up front, with the assertions in subtests below it.
// Every header comes from that one response, so re-requesting per case would
// just be slower; the t.Run loop is here for the naming, so a failure says
// which header is missing rather than "got: ; want: nosniff".
func TestSecureHeaders(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	// The healthcheck is used because it's public — no token, no database row,
	// nothing to set up. The middleware wraps every route identically, so any
	// endpoint would do.
	_, headers, _ := ts.get(t, "/v1/healthcheck", "")

	tests := []struct {
		header string
		want   string
	}{
		// Stops a browser from second-guessing our Content-Type. Without it, a
		// browser that decides a JSON response "looks like" HTML may execute it
		// as HTML — which turns any string a user can get stored and echoed back
		// into a cross-site scripting vector.
		{header: "X-Content-Type-Options", want: "nosniff"},

		// Refuses to be loaded in a frame or iframe, which is what defeats
		// clickjacking: an attacker's page can't invisibly overlay ours and
		// trick a logged-in user into clicking something.
		{header: "X-Frame-Options", want: "deny"},

		// Sends the full referring URL only to same-origin destinations; a
		// cross-origin request gets the bare origin. Without it, a path like
		// /v1/users/activated?token=... could leak in the Referer header of any
		// outbound link.
		{header: "Referrer-Policy", want: "origin-when-cross-origin"},

		// The strictest possible CSP: this API returns JSON, so a response has
		// no legitimate reason to load a script, stylesheet, image or font from
		// anywhere. `default-src 'none'` forbids all of it, and
		// `frame-ancestors 'none'` is the modern equivalent of X-Frame-Options
		// (both are sent, because older browsers only understand the latter).
		{header: "Content-Security-Policy", want: "default-src 'none'; frame-ancestors 'none'"},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			// http.Header.Get is case-insensitive, so the exact casing used in
			// the middleware doesn't matter here — HTTP header names are
			// case-insensitive by spec and Go canonicalises them.
			assert.Equal(t, headers.Get(tt.header), tt.want)
		})
	}
}
