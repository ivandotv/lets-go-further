package main

import "net/http"

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
