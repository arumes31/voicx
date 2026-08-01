// i18n_test.go verifies the en/de message catalogs in
// frontend/src/i18n.js carry the same keys (336).
package main

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// catalogKeys extracts the keys of one catalog block (`const xx = { ... };`).
func catalogKeys(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)const ` + name + ` = \{(.*?)\n\};`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("catalog %s not found in i18n.js", name)
	}
	keyRe := regexp.MustCompile(`"([a-z0-9.]+)":`)
	var keys []string
	for _, km := range keyRe.FindAllStringSubmatch(m[1], -1) {
		keys = append(keys, km[1])
	}
	sort.Strings(keys)
	return keys
}

func TestI18nCatalogParity(t *testing.T) {
	raw, err := os.ReadFile("frontend/src/i18n.js")
	if err != nil {
		t.Fatalf("read i18n.js: %v", err)
	}
	src := string(raw)
	en := catalogKeys(t, src, "en")
	de := catalogKeys(t, src, "de")
	if len(en) == 0 {
		t.Fatal("en catalog empty")
	}
	if len(en) != len(de) {
		t.Fatalf("catalog size mismatch: en=%d de=%d", len(en), len(de))
	}
	for i := range en {
		if en[i] != de[i] {
			t.Fatalf("key mismatch at %d: en has %q, de has %q", i, en[i], de[i])
		}
	}
}
