// links.go implements expiring download links (267): the control channel
// issues a token URL (MsgFileLink) served by the health HTTP server at
// /dl/<token>. Links are LAN-friendly: anyone with the URL can download the
// file until expiry (15 minutes); no separate auth runs on the HTTP path.
package filetransfer

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// LinkTTL is how long a download link stays valid.
const LinkTTL = 15 * time.Minute

// link is one issued download link.
type link struct {
	path    string // path relative to the registry's confined root
	name    string // download file name (Content-Disposition)
	expires time.Time
}

// LinkRegistry tracks issued download links and serves them over HTTP. It
// implements http.Handler mounted at /dl/ on the health server.
type LinkRegistry struct {
	mu      sync.Mutex
	rootDir string
	links   map[string]link
}

// NewLinkRegistry returns an empty registry.
func NewLinkRegistry(rootDir string) *LinkRegistry {
	return &LinkRegistry{rootDir: rootDir, links: map[string]link{}}
}

// Create issues a link for a regular file beneath the configured root and
// returns its token. path must be relative to that root.
func (r *LinkRegistry) Create(path, name string) (string, time.Time, error) {
	// /dl/<token> holds no key, so a chat attachment would be served as raw
	// ciphertext that looks like a corrupt download (91-135).
	if isEncryptedAttachment(name) {
		return "", time.Time{}, ErrEncryptedAttachment
	}
	root, err := os.OpenRoot(r.rootDir)
	if err != nil {
		return "", time.Time{}, errors.New("file not found on disk")
	}
	defer func() { _ = root.Close() }()
	f, err := openRegularBlob(root, path)
	if err != nil {
		return "", time.Time{}, errors.New("file not found on disk")
	}
	if err := f.Close(); err != nil {
		return "", time.Time{}, errors.New("file not found on disk")
	}
	token, err := randomHex(16)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(LinkTTL)
	r.mu.Lock()
	r.links[token] = link{path: path, name: name, expires: expires}
	r.mu.Unlock()
	return token, expires, nil
}

// take resolves a token and prunes expired links lazily.
func (r *LinkRegistry) take(token string) (link, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for t, l := range r.links {
		if now.After(l.expires) {
			delete(r.links, t)
		}
	}
	l, ok := r.links[token]
	return l, ok
}

// ServeHTTP serves GET /dl/<token>.
func (r *LinkRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := filepath.Base(req.URL.Path)
	l, ok := r.take(token)
	if !ok {
		http.Error(w, "unknown or expired link", http.StatusNotFound)
		return
	}
	root, err := os.OpenRoot(r.rootDir)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer func() { _ = root.Close() }()
	f, err := openRegularBlob(root, l.path)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(l.name))
	http.ServeContent(w, req, l.name, info.ModTime(), f)
}
