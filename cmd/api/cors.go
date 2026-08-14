package main

import "net/http"

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

				// Without this, browser JavaScript cannot READ X-Request-Id.
				// A cross-origin response only exposes a handful of
				// safelisted headers (Content-Type, Cache-Control and a few
				// others) to script; anything else is invisible unless it's
				// named here — the header is still sent, the browser just
				// hides it. Since the whole point of the ID is that a client
				// can quote it when reporting a problem, omitting this would
				// make the feature useless to exactly the callers it's for.
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")

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
