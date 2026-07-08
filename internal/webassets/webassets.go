// Package webassets serves the shared front-end assets (Pico CSS, the
// ggscale stylesheet, and web fonts) used by both the control panel and the
// player-facing site. Embedding them once and mounting at a single
// origin-relative path (/v1/assets) keeps player pages styled even when the
// control panel surface is disabled, and avoids duplicating the font files.
package webassets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

//go:embed static
var staticFS embed.FS

// mountPath is where internal/httpapi mounts Handler.
const mountPath = "/v1/assets/"

// versions maps each embedded file (keyed by URL name, e.g. "app.css") to a
// short content hash, computed once at startup. URL appends it as a query so
// the immutable Cache-Control stays truthful: edit a file and every page
// links a new URL, so browsers can't keep serving the year-old copy.
var versions = computeVersions()

func computeVersions() map[string]string {
	out := map[string]string{}
	_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := staticFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(data)
		out[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:4])
		return nil
	})
	return out
}

// URL returns the mounted, cache-busted URL for an embedded asset, e.g.
// /v1/assets/app.css?v=1a2b3c4d. Unknown names come back unversioned.
func URL(name string) string {
	u := mountPath + name
	if v, ok := versions[name]; ok {
		u += "?v=" + v
	}
	return u
}

// Handler serves the embedded assets under a "/*" wildcard, e.g. when mounted
// at /v1/assets it answers /v1/assets/pico.min.css.
func Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		Serve(w, req, staticFS, "static")
	})
	return r
}

// Serve writes the chi "/*" wildcard asset from root inside fsys with the
// shared cache and nosniff headers. The control panel's own JS assets are served
// through it too, so the traversal guard and cache policy live in one place.
func Serve(w http.ResponseWriter, r *http.Request, fsys fs.FS, root string) {
	name := chi.URLParam(r, "*")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFileFS(w, r, fsys, root+"/"+name)
}
