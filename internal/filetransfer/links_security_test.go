package filetransfer

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLinkRegistry(t *testing.T, data []byte) (*LinkRegistry, string) {
	t.Helper()
	root := t.TempDir()
	const rel = "7/shared.txt"
	if err := os.MkdirAll(filepath.Join(root, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewLinkRegistry(root), rel
}

func TestLinkRegistryBoundsAndPrunesActiveLinks(t *testing.T) {
	registry, rel := newTestLinkRegistry(t, []byte("bounded"))
	registry.maxLinks = 2
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return now }
	tokens := []string{
		strings.Repeat("a", linkTokenHexLength),
		strings.Repeat("b", linkTokenHexLength),
		strings.Repeat("c", linkTokenHexLength),
	}
	registry.newToken = func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}

	for range 2 {
		if _, _, err := registry.Create(rel, "shared.txt"); err != nil {
			t.Fatalf("Create within capacity: %v", err)
		}
	}
	if _, _, err := registry.Create(rel, "shared.txt"); !errors.Is(err, ErrTooManyLinks) {
		t.Fatalf("Create over capacity error = %v, want ErrTooManyLinks", err)
	}

	now = now.Add(LinkTTL)
	if _, _, err := registry.Create(rel, "shared.txt"); err != nil {
		t.Fatalf("Create after expiry pruning: %v", err)
	}
	if len(registry.links) != 1 {
		t.Fatalf("active links after pruning = %d, want 1", len(registry.links))
	}
}

func TestLinkRegistryRetriesTokenCollisions(t *testing.T) {
	registry, rel := newTestLinkRegistry(t, []byte("collision-safe"))
	first := strings.Repeat("a", linkTokenHexLength)
	second := strings.Repeat("b", linkTokenHexLength)
	generated := []string{first, first, second}
	registry.newToken = func() (string, error) {
		token := generated[0]
		generated = generated[1:]
		return token, nil
	}

	gotFirst, _, err := registry.Create(rel, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, _, err := registry.Create(rel, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst != first || gotSecond != second || len(registry.links) != 2 {
		t.Fatalf("collision results = %q/%q, links = %d", gotFirst, gotSecond, len(registry.links))
	}
}

func TestDownloadLinkRejectsMalformedPathsAndMethods(t *testing.T) {
	registry, rel := newTestLinkRegistry(t, []byte("strict path"))
	token, _, err := registry.Create(rel, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/dl/",
		"/dl/not-hex",
		"/other/" + token,
		"/dl/" + token + "/extra",
		"/dl/" + token + `\extra`,
		"/dl/" + strings.ToUpper(token),
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("GET %q status = %d, want 404", path, recorder.Code)
			}
		})
	}

	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/dl/"+token, nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST status/Allow = %d/%q", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func TestDownloadLinkSecurityHeadersAndDisposition(t *testing.T) {
	registry, rel := newTestLinkRegistry(t, []byte("download"))
	token, _, err := registry.Create(rel, `résumé "2026".txt`)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dl/"+token, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body, err := io.ReadAll(recorder.Result().Body)
	if err != nil || string(body) != "download" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	for name, want := range map[string]string{
		"Cache-Control":           "private, no-store",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || strings.ContainsAny(disposition, "\r\n") {
		t.Fatalf("unsafe Content-Disposition = %q", disposition)
	}
}

func TestDownloadLinkExpiresAtDeadline(t *testing.T) {
	registry, rel := newTestLinkRegistry(t, []byte("expired"))
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return now }
	token, expires, err := registry.Create(rel, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	now = expires
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dl/"+token, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expired link status = %d, want 404", recorder.Code)
	}
}
