package secrets_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
	"github.com/jayleaton/ai-patterns-go/packages/secrets/reveal"
)

// These run the Doppler adapter over a real socket (httptest.Server).

func TestDopplerDownloadOverHTTP(t *testing.T) {
	t.Parallel()

	f := newFake(map[string]string{key: "re_live", "DOPPLER_CONFIG": "prd"})
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()

	d := secrets.NewDoppler(secrets.NewSecret(testToken))
	if d.APIURL != secrets.DefaultDopplerURL || d.Name() != "doppler" {
		t.Fatalf("defaults: %+v", d)
	}
	d.APIURL = srv.URL + "/" // trailing slash is tolerated

	snap, err := d.Fetch(t.Context(), secrets.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := snap.Get(key); reveal.Value(v) != "re_live" || snap.Revision() != `"v1"` || gotAccept != "application/json" {
		t.Fatalf("snapshot names=%v revision=%s accept=%s", snap.Names(), snap.Revision(), gotAccept)
	}
	if names := snap.Names(); len(names) != 2 || names[0] != "DOPPLER_CONFIG" {
		t.Fatalf("names = %v", names)
	}

	// Conditional request: 304 returns the previous snapshot as is.
	again, err := d.Fetch(t.Context(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, notMod := f.stats(); notMod != 1 || again.Revision() != snap.Revision() || again.Len() != 2 {
		t.Fatalf("304 handling: notMod=%d revision=%s", notMod, again.Revision())
	}

	// A zero-value Doppler uses a default client and still works against APIURL.
	if _, err := (&secrets.Doppler{Token: secrets.NewSecret(testToken), APIURL: srv.URL}).Fetch(t.Context(), secrets.Snapshot{}); err != nil {
		t.Fatal(err)
	}
}

func TestDopplerErrors(t *testing.T) {
	t.Parallel()

	// Each subtest gets its own fake and server, so they run in parallel.
	at := func(token string) func(*httptest.Server) *secrets.Doppler {
		return func(srv *httptest.Server) *secrets.Doppler {
			return &secrets.Doppler{Token: secrets.NewSecret(token), APIURL: srv.URL, HTTPClient: srv.Client()}
		}
	}
	fixed := func(d *secrets.Doppler) func(*httptest.Server) *secrets.Doppler {
		return func(*httptest.Server) *secrets.Doppler { return d }
	}

	tests := []struct {
		name  string
		setup func(*fakeDoppler)
		d     func(*httptest.Server) *secrets.Doppler
		want  string
	}{
		{"missing token", func(*fakeDoppler) {}, at(""), "missing token"},
		{"bad token", func(*fakeDoppler) {}, at("dp.st.wrong_token"), "token rejected (401 Unauthorized)"},
		{"server error", func(f *fakeDoppler) { f.fail(500, "") }, at(testToken), "unexpected status 500"},
		{"oversize", func(f *fakeDoppler) { f.setRaw(`{"A":"` + strings.Repeat("x", 1<<20) + `"}`) }, at(testToken), "larger than"},
		{"unreachable", func(*fakeDoppler) {}, fixed(&secrets.Doppler{Token: secrets.NewSecret(testToken), APIURL: "http://user:pass@127.0.0.1:1"}), "doppler: request"},
		{"bad url", func(*fakeDoppler) {}, fixed(&secrets.Doppler{Token: secrets.NewSecret(testToken), APIURL: "http://[::1"}), "build request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFake(map[string]string{key: "re_secret_in_body"})
			srv := httptest.NewServer(f)
			defer srv.Close()
			tt.setup(f)
			_, err := tt.d(srv).Fetch(t.Context(), secrets.Snapshot{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			for _, leak := range []string{"re_secret_in_body", testToken, "dp.st.wrong_token", "pass@"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error leaks %q: %v", leak, err)
				}
			}
		})
	}
}

func TestDopplerRateLimit(t *testing.T) {
	t.Parallel()

	f := newFake(map[string]string{key: "re_a"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &secrets.Doppler{Token: secrets.NewSecret(testToken), APIURL: srv.URL, HTTPClient: srv.Client()}

	for _, tt := range []struct {
		header string
		min    time.Duration
		max    time.Duration
	}{
		{"7", 7 * time.Second, 7 * time.Second},
		{time.Now().Add(time.Minute).UTC().Format(http.TimeFormat), 50 * time.Second, time.Minute},
		{"soon", 0, 0},
	} {
		f.fail(http.StatusTooManyRequests, tt.header)
		_, err := d.Fetch(t.Context(), secrets.Snapshot{})
		ra, ok := errors.AsType[secrets.RetryAfter](err)
		if !ok || ra.RetryAfter() < tt.min || ra.RetryAfter() > tt.max || !strings.Contains(err.Error(), "429") {
			t.Fatalf("Retry-After %q: err=%v", tt.header, err)
		}
	}
}

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()

	s := secrets.NewSecret("re_plain_value")
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "s", s, "group", slog.GroupValue(slog.Any("s", s)))
	j, _ := json.Marshal(map[string]secrets.Secret{"k": s})
	text, _ := s.MarshalText()
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%s %v %+v %#v %d %x %q", s, s, s, s, s, s, s),
		fmt.Sprintf("%v", struct{ K secrets.Secret }{s}), s.String(), sb.String(), string(j), string(text),
	} {
		if strings.Contains(out, "re_plain_value") || !strings.Contains(out, "[redacted]") {
			t.Fatalf("secret printed: %s", out)
		}
	}
	if reveal.Value(s) != "re_plain_value" || s.Len() != len("re_plain_value") || s.IsZero() || !s.Equal(secrets.NewSecret("re_plain_value")) || s.Equal(secrets.NewSecret("x")) {
		t.Fatal("reveal.Value/Len/IsZero/Equal")
	}
	fp := s.Fingerprint()
	if !strings.HasPrefix(fp, "fp:") || len(fp) != 11 || fp != secrets.NewSecret("re_plain_value").Fingerprint() || fp == secrets.NewSecret("re_other").Fingerprint() {
		t.Fatalf("fingerprint %s", fp)
	}
	if (secrets.Secret{}).Fingerprint() != "none" || !(secrets.Secret{}).IsZero() {
		t.Fatal("zero secret")
	}

	// Types holding secrets in unexported fields redact themselves too.
	snap := secrets.NewSnapshot(map[string]string{"K": "re_plain_value"}, `"v1"`)
	d := secrets.NewDoppler(secrets.NewSecret("dp.st.plain_token"))
	m := secrets.NewManager(secrets.NewEnv(map[string]string{"K": "re_plain_value"}), secrets.Options{})
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	sb.Reset()
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "snap", snap, "doppler", d, "m", m)
	for _, out := range []string{
		fmt.Sprintf("%v %+v %#v %s", snap, snap, snap, snap), fmt.Sprintf("%v %+v %#v", d, d, d),
		fmt.Sprintf("%v %+v %#v", m, m, m), sb.String(),
	} {
		if strings.Contains(out, "re_plain_value") || strings.Contains(out, "plain_token") {
			t.Fatalf("value printed: %s", out)
		}
	}
	if !strings.Contains(fmt.Sprint(snap), "names: [K]") || !strings.Contains(sb.String(), "snap.names=[K]") {
		t.Fatalf("snapshot format lacks names: %s / %s", snap, sb.String())
	}
}

func TestDopplerDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	d := secrets.NewDoppler(secrets.NewSecret(testToken))
	d.APIURL = srv.URL
	if _, err := d.Fetch(t.Context(), secrets.Snapshot{}); err == nil || !strings.Contains(err.Error(), "302") || followed {
		t.Fatalf("err = %v, followed = %v", err, followed)
	}
	// The zero-value client refuses redirects too.
	if _, err := (&secrets.Doppler{Token: secrets.NewSecret(testToken), APIURL: srv.URL}).Fetch(t.Context(), secrets.Snapshot{}); err == nil || followed {
		t.Fatalf("zero-value client: err = %v, followed = %v", err, followed)
	}
}
