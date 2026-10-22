// Package static embeds the UI's browser assets: the generated Tailwind CSS and
// the pinned Datastar bundle (no CDN).
package static

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/web/ui/routes"
)

// Datastar bundle pin. Update all three together; TestDatastarPin checks the
// embedded file against DatastarSHA256.
// Source: https://raw.githubusercontent.com/starfederation/datastar/v1.0.4/bundles/datastar.js
const (
	DatastarVersion = "1.0.4"
	DatastarFile    = "datastar-1.0.4.js"
	DatastarSHA256  = "727844adfc825ee651fb93c544a2a739986f9a21820a94524b35f0cac470cf91"
)

// FaviconFile is the original template's favicon, also served at /favicon.ico.
const FaviconFile = "favicon.ico"

// CSSFile is written by the Tailwind standalone CLI (see apps/web/ui/README.md).
const CSSFile = "app.css"

//go:embed app.css datastar-1.0.4.js favicon.ico
var FS embed.FS

var versions = func() map[string]string {
	out := map[string]string{}
	_ = fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := FS.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[path] = hex.EncodeToString(sum[:8])
		return nil
	})
	return out
}()

// URL returns the cache-busted URL of an embedded asset.
func URL(name string) string {
	return routes.Static + name + "?v=" + versions[name]
}

// Handler serves the embedded assets under routes.Static. Requests carrying
// the content version are cached immutably. Only the embedded files are
// served: no directory listings, 404 for anything else.
func Handler() http.Handler {
	files := http.StripPrefix(routes.Static, http.FileServerFS(FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, routes.Static)
		if _, ok := versions[name]; !ok {
			http.NotFound(w, r)
			return
		}
		if v := r.URL.Query().Get("v"); v != "" && v == versions[name] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
