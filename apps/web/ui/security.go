package ui

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/a-h/templ"
)

// contentSecurityPolicy allows only this origin's own assets plus the
// per-request nonce. Datastar compiles data-* expressions into nonced
// <script> elements when <html data-nonce> is set (view.Layout), so neither
// 'unsafe-eval' nor 'unsafe-inline' is needed. Templates must not use inline
// style="" attributes or inline handlers: show/hide with class="hidden" and
// data-class:hidden.
func contentSecurityPolicy(nonce string) string {
	return "default-src 'none'; " +
		"script-src 'self' 'nonce-" + nonce + "'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"manifest-src 'self'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"object-src 'none'"
}

// secure sets the browser security headers on every UI response and puts a
// fresh CSP nonce on the request context (templ.GetNonce). Pages and
// Datastar streams are never stored (they carry user data and emailed
// tokens), whatever the handler or the SSE writer set; the static handler
// sets its own Cache-Control for assets. hsts is set behind https.
func secure(next http.Handler, hsts bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newNonce()
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin")
		h.Set("Permissions-Policy", "accelerometer=(), browsing-topics=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(&noStore{ResponseWriter: w}, r.WithContext(templ.WithNonce(r.Context(), nonce)))
	})
}

// noStore forces Cache-Control: no-store on HTML and SSE responses when
// their header is written (datastar.NewSSE sets no-cache).
type noStore struct {
	http.ResponseWriter
	done bool
}

func (n *noStore) header() {
	if n.done {
		return
	}
	n.done = true
	ct := n.Header().Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "text/event-stream") {
		n.Header().Set("Cache-Control", "no-store")
	}
}

func (n *noStore) WriteHeader(code int) {
	n.header()
	n.ResponseWriter.WriteHeader(code)
}

func (n *noStore) Write(b []byte) (int, error) {
	n.header()
	return n.ResponseWriter.Write(b)
}

// FlushError lets http.ResponseController flush SSE through the wrapper.
func (n *noStore) FlushError() error {
	n.header()
	return http.NewResponseController(n.ResponseWriter).Flush()
}

func (n *noStore) Unwrap() http.ResponseWriter { return n.ResponseWriter }

func newNonce() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b) // never fails on supported platforms (crypto/rand docs)
	return base64.RawStdEncoding.EncodeToString(b)
}

// strict answers every request mux has no route for (unknown path, or a
// known path with another method) with a bare 404: no 405, no Allow header,
// nothing that maps the routes.
func strict(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
