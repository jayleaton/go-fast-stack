package static

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDatastarPin(t *testing.T) {
	t.Parallel()
	b, err := FS.ReadFile(DatastarFile)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != DatastarSHA256 {
		t.Fatalf("%s sha256 = %s, pinned %s", DatastarFile, got, DatastarSHA256)
	}
	if !strings.HasPrefix(string(b), "// Datastar v"+DatastarVersion+"\n") {
		t.Fatalf("%s header does not say v%s", DatastarFile, DatastarVersion)
	}
}

func TestHandlerCaching(t *testing.T) {
	t.Parallel()
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, URL(CSSFile), nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned: code %d cache %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), "tailwindcss") {
		t.Fatal("app.css is not the Tailwind build")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/"+CSSFile+"?v=stale", nil))
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("stale version cached: %q", rec.Header().Get("Cache-Control"))
	}
}
