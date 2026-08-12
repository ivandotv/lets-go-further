package main

import (
	"testing"

	"greenlight/internal/assert"
)

func TestSecureHeaders(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	_, headers, _ := ts.get(t, "/v1/healthcheck", "")

	tests := []struct {
		header string
		want   string
	}{
		{header: "X-Content-Type-Options", want: "nosniff"},
		{header: "X-Frame-Options", want: "deny"},
		{header: "Referrer-Policy", want: "origin-when-cross-origin"},
		{header: "Content-Security-Policy", want: "default-src 'none'; frame-ancestors 'none'"},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			assert.Equal(t, headers.Get(tt.header), tt.want)
		})
	}
}
