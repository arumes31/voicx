// links.go implements expiring download links (267): the control channel
// issues a token URL (MsgFileLink) served by the health HTTP server at
// /dl/<token>. Links are LAN-friendly: anyone with the URL can download the
// file until expiry (15 minutes); no separate auth runs on the HTTP path.
package filetransfer

import (
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// LinkTTL is how long a download link stays valid.
const LinkTTL = 15 * time.Minute

const (
	maxActiveLinks     = 4096
	linkTokenBytes     = 16
	linkTokenHexLength = linkTokenBytes * 2
	linkTokenAttempts  = 4
)

// ErrTooManyLinks prevents an unbounded in-memory link registry.
var ErrTooManyLinks = errors.New("too many active download links")

// link is one issued download link.
type link struct {
	path    string // path relative to the registry's confined root
	name    string // download file name (Content-Disposition)
	expires time.Time
}

// LinkRegistry tracks issued download links and serves them over HTTP. It
// implements http.Handler mounted at /dl/ on the health server.
type LinkRegistry struct {
	mu       sync.Mutex
	rootDir  string
	links    map[string]link
	maxLinks int
	now      func() time.Time
	newToken func() (string, error)
}

// NewLinkRegistry returns an empty registry.
func NewLinkRegistry(rootDir string) *LinkRegistry {
	return &LinkRegistry{
		rootDir:  rootDir,
		links:    map[string]link{},
		maxLinks: maxActiveLinks,
		now:      time.Now,
		newToken: func() (string, error) { return randomHex(linkTokenBytes) },
	}
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
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneExpiredLocked(now)
	if len(r.links) >= r.maxLinks {
		return "", time.Time{}, ErrTooManyLinks
	}
	for range linkTokenAttempts {
		token, err := r.newToken()
		if err != nil {
			return "", time.Time{}, err
		}
		if !validLinkToken(token) {
			return "", time.Time{}, errors.New("download link token generator returned an invalid token")
		}
		if _, collision := r.links[token]; collision {
			continue
		}
		expires := now.Add(LinkTTL)
		r.links[token] = link{path: path, name: name, expires: expires}
		return token, expires, nil
	}
	return "", time.Time{}, errors.New("download link token collision limit exceeded")
}

// take resolves a token and prunes expired links lazily.
func (r *LinkRegistry) take(token string) (link, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneExpiredLocked(r.now())
	l, ok := r.links[token]
	return l, ok
}

func (r *LinkRegistry) pruneExpiredLocked(now time.Time) {
	for t, l := range r.links {
		if !now.Before(l.expires) {
			delete(r.links, t)
		}
	}
}

// ServeHTTP serves GET /dl/<token>.
func (r *LinkRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, valid := linkTokenFromPath(req.URL.Path)
	if !valid {
		http.Error(w, "unknown or expired link", http.StatusNotFound)
		return
	}
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
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": l.name})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition)
	http.ServeContent(w, req, l.name, info.ModTime(), f)
}

func linkTokenFromPath(path string) (string, bool) {
	const prefix = "/dl/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(path, prefix)
	if !validLinkToken(token) {
		return "", false
	}
	return token, true
}

func validLinkToken(token string) bool {
	if len(token) != linkTokenHexLength || token != strings.ToLower(token) {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}
