package secrets_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
	"github.com/jayleaton/go-fast-stack/packages/secrets/reveal"
)

const testToken = "dp.st.test_token_value"

// fakeDoppler is the Download API of one config: bearer auth, ETag /
// If-None-Match, and switches for outages, rate limits and raw bodies.
type fakeDoppler struct {
	mu         sync.Mutex
	raw        string // response body
	etag       string
	version    int
	status     int // non-200 status to return instead of the body
	retryAfter string
	calls      int
	times      []time.Time // when each request arrived (fake clock in synctest)
	notMod     int         // 304s served
}

func newFake(values map[string]string) *fakeDoppler {
	f := &fakeDoppler{}
	f.set(values)
	return f
}

// set publishes a new config version.
func (f *fakeDoppler) set(values map[string]string) {
	b, _ := json.Marshal(values)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.raw, f.etag, f.status = string(b), `"v`+strconv.Itoa(f.version)+`"`, 0
}

// setRaw publishes a raw (possibly malformed) body.
func (f *fakeDoppler) setRaw(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.raw, f.etag, f.status = raw, `"v`+strconv.Itoa(f.version)+`"`, 0
}

func (f *fakeDoppler) fail(status int, retryAfter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.retryAfter = status, retryAfter
}

func (f *fakeDoppler) recover() { f.fail(0, "") }

func (f *fakeDoppler) stats() (calls, notModified int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.notMod
}

func (f *fakeDoppler) requestTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.times...)
}

func (f *fakeDoppler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.times = append(f.times, time.Now())
	if r.URL.Path != "/v3/configs/config/secrets/download" || r.URL.Query().Get("format") != "json" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"messages":["Invalid Auth token"]}`, http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		http.Error(w, `{"messages":["`+f.raw+`"]}`, f.status) // a body that echoes values must never surface
		return
	}
	w.Header().Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		f.notMod++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, f.raw)
}

// inMemory serves requests straight from a handler, so the Manager can run in
// a synctest bubble (fake clock) with no sockets.
type inMemory struct{ h http.Handler }

func (t inMemory) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func dopplerFor(f *fakeDoppler) *secrets.Doppler {
	return &secrets.Doppler{Token: secrets.NewSecret(testToken), APIURL: "http://doppler.fake", HTTPClient: &http.Client{Transport: inMemory{f}}}
}

// logBuffer is a concurrency-safe log sink.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLogger() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// recorder is a Hook that keeps every value it was called with.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) hook(v secrets.Secret) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, reveal.Value(v))
	return nil
}

func (r *recorder) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// advance advances the synctest fake clock; only valid inside a synctest bubble.
// Tests never sleep in real time: outside a bubble this would block.
func advance(d time.Duration) {
	time.Sleep(d) //nolint:forbidigo // synctest: advances the fake clock, no real time passes
}
