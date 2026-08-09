// Package console embeds and serves the built admin SPA (Vite + React + Semi-UI, source
// in pool_server/web-spa). It is mounted at /console/; the legacy vanilla-JS UI stays
// available under /legacy/ as an operator fallback. Build the SPA with
// `npm --prefix web-spa run build`, which emits into this package's dist/.
package console

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

type compressedAsset struct {
	body        []byte
	contentType string
}

const consoleBuildErrorHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Console unavailable</title>
  <style>
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f6f8fa; color: #1f2328; }
    main { width: min(520px, calc(100% - 32px)); padding: 24px; border: 1px solid #d8dee4; border-radius: 8px; background: #fff; box-shadow: 0 12px 32px rgba(31, 35, 40, 0.08); }
    h1 { margin: 0 0 8px; font-size: 20px; line-height: 1.3; }
    p { margin: 0; color: #57606a; line-height: 1.6; }
    p + p { margin-top: 12px; }
    a { color: #0969da; font-weight: 600; }
    code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 0.92em; }
  </style>
</head>
<body>
  <main>
    <h1>Console is unavailable</h1>
    <p>The embedded SPA build is missing or incomplete. Run <code>npm --prefix web-spa run build</code>, verify the release manifest, and restart the server.</p>
    <p><a href="/legacy/">Open the legacy console</a> while the release is repaired.</p>
  </main>
</body>
</html>`

var consoleAssetReferenceRE = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["'](/console/[^"'?#]+)`)

// Handler serves the SPA under /console/. Real assets are served from the embedded FS;
// any other sub-path falls back to index.html so react-router (basename /console) handles
// deep links and refreshes client-side.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveConsoleBuildError(w)
		})
	}
	index, ixErr := fs.ReadFile(sub, "index.html")
	if ixErr == nil {
		ixErr = validateConsoleIndexAssets(sub, index)
	}
	gzippedAssets := buildCompressedAssets(sub)
	fileServer := http.StripPrefix("/console/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ixErr != nil {
			serveConsoleBuildError(w)
			return
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/console/"), "/")
		if rel == "" || rel == "index.html" {
			serveIndex(w, index)
			return
		}
		if st, err := fs.Stat(sub, rel); err == nil && !st.IsDir() {
			setAssetCacheHeaders(w, rel)
			if asset, ok := gzippedAssets[rel]; ok && canServeGzip(r) {
				serveCompressedAsset(w, r, asset)
				return
			}
			fileServer.ServeHTTP(w, r) // real asset (js/css/img)
			return
		}
		if strings.HasPrefix(rel, "assets/") {
			// A missing immutable asset is never a client-side route. Returning the
			// SPA shell here produces a misleading 200 text/html response and a blank
			// page when the browser enforces module MIME types.
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		serveIndex(w, index) // SPA deep link → client-side route
	})
}

func validateConsoleIndexAssets(sub fs.FS, index []byte) error {
	matches := consoleAssetReferenceRE.FindAllSubmatch(index, -1)
	if len(matches) == 0 {
		return fmt.Errorf("console index contains no local assets")
	}
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		rel := strings.TrimPrefix(string(match[1]), "/console/")
		if !fs.ValidPath(rel) {
			return fmt.Errorf("console index contains invalid asset path %q", rel)
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		st, err := fs.Stat(sub, rel)
		if err != nil {
			return fmt.Errorf("console index asset %q is missing: %w", rel, err)
		}
		if st.IsDir() {
			return fmt.Errorf("console index asset %q is a directory", rel)
		}
	}
	return nil
}

func serveConsoleBuildError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(consoleBuildErrorHTML))
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	_, _ = w.Write(index)
}

func setAssetCacheHeaders(w http.ResponseWriter, rel string) {
	if strings.HasPrefix(rel, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if isCompressibleAsset(rel) {
			w.Header().Add("Vary", "Accept-Encoding")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

func buildCompressedAssets(sub fs.FS) map[string]compressedAsset {
	assets := map[string]compressedAsset{}
	_ = fs.WalkDir(sub, "assets", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isCompressibleAsset(rel) {
			return nil
		}
		raw, err := fs.ReadFile(sub, rel)
		if err != nil || len(raw) < 1024 {
			return nil
		}
		var buf bytes.Buffer
		// Assets are compressed exactly ONCE at boot and served from memory forever,
		// so spend the CPU on BestCompression (smaller wire transfer for the 1.3MB+
		// JS/CSS bundles) rather than BestSpeed — the cost is paid once at startup.
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil
		}
		if _, err := zw.Write(raw); err != nil {
			_ = zw.Close()
			return nil
		}
		if err := zw.Close(); err != nil {
			return nil
		}
		assets[rel] = compressedAsset{
			body:        buf.Bytes(),
			contentType: contentTypeForAsset(rel, raw),
		}
		return nil
	})
	return assets
}

func isCompressibleAsset(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".css", ".js", ".json", ".map", ".svg", ".txt", ".html":
		return true
	default:
		return false
	}
}

func canServeGzip(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Range") != "" {
		return false
	}
	for _, value := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		encoding := strings.TrimSpace(strings.Split(value, ";")[0])
		if strings.EqualFold(encoding, "gzip") {
			return true
		}
	}
	return false
}

func serveCompressedAsset(w http.ResponseWriter, r *http.Request, asset compressedAsset) {
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(asset.body)
	}
}

func contentTypeForAsset(rel string, raw []byte) string {
	if typ := mime.TypeByExtension(path.Ext(rel)); typ != "" {
		return typ
	}
	if len(raw) > 512 {
		raw = raw[:512]
	}
	return http.DetectContentType(raw)
}
