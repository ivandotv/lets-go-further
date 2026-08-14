package main

import (
	"expvar"
	"net/http"
	"strconv"

	"github.com/felixge/httpsnoop"
	"github.com/tomasen/realip"
)

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
			"request_id", app.contextGetRequestID(r),
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
