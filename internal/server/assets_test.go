package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testTinyJPEG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 1, 1))
	pixels.Set(0, 0, color.RGBA{R: 0x7f, G: 0x40, B: 0x20, A: 0xff})
	if err := jpeg.Encode(&encoded, pixels, nil); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return encoded.Bytes()
}

func testAnimatedGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 1, 1), palette)
	second := image.NewPaletted(image.Rect(0, 0, 1, 1), palette)
	second.SetColorIndex(0, 0, 1)
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{
		Image: []*image.Paletted{first, second},
		Delay: []int{0, 1},
	}); err != nil {
		t.Fatalf("encode animated test GIF: %v", err)
	}
	return encoded.Bytes()
}

func testPNGWithChunk(t *testing.T, chunkType string, data []byte) []byte {
	t.Helper()
	if len(chunkType) != 4 {
		t.Fatalf("PNG chunk type %q must be four bytes", chunkType)
	}
	iend := bytes.LastIndex(tinyPNG, []byte("IEND"))
	if iend < 4 {
		t.Fatal("test PNG has no IEND chunk")
	}
	iend -= 4 // Include IEND's length field in the retained suffix.

	chunk := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(data)))
	copy(chunk[4:8], chunkType)
	copy(chunk[8:8+len(data)], data)
	binary.BigEndian.PutUint32(chunk[8+len(data):], crc32.ChecksumIEEE(chunk[4:8+len(data)]))

	raw := make([]byte, 0, len(tinyPNG)+len(chunk))
	raw = append(raw, tinyPNG[:iend]...)
	raw = append(raw, chunk...)
	raw = append(raw, tinyPNG[iend:]...)
	return raw
}

func TestAvatarAssetBaseIsDeterministicAndLocal(t *testing.T) {
	ids := []string{
		"",
		"ordinary-user",
		"teamspeak/id+with=base64",
		"../unix/traversal",
		`..\windows\traversal`,
		`C:\Windows\system32\image`,
		"/etc/secret",
	}
	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		got := avatarAssetBase(id)
		if again := avatarAssetBase(id); again != got {
			t.Fatalf("avatarAssetBase(%q) changed from %q to %q", id, got, again)
		}
		if len(got) != sha256.Size*2 {
			t.Fatalf("avatarAssetBase(%q) length = %d", id, len(got))
		}
		if _, err := hex.DecodeString(got); err != nil {
			t.Fatalf("avatarAssetBase(%q) = %q: %v", id, got, err)
		}
		sum := sha256.Sum256([]byte(id))
		if want := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("avatarAssetBase(%q) = %q, want SHA-256 %q", id, got, want)
		}
		if !filepath.IsLocal(got) || filepath.Base(got) != got || strings.ContainsAny(got, `/\`) {
			t.Fatalf("avatarAssetBase(%q) is not local: %q", id, got)
		}
		if prior, exists := seen[got]; exists {
			t.Fatalf("test IDs %q and %q mapped to the same name", prior, id)
		}
		seen[got] = id
	}
}

func TestEmojiNameValidationIsStrict(t *testing.T) {
	valid := []string{"a", "party-time_2", strings.Repeat("x", 32)}
	for _, name := range valid {
		if !emojiNameRe.MatchString(name) {
			t.Errorf("emoji name %q was rejected", name)
		}
	}
	invalid := []string{
		"", strings.Repeat("x", 33), "Party", "party time", "../party",
		`..\party`, "party.png", ":party:", "fire🔥", "a/b", `a\b`,
	}
	for _, name := range invalid {
		if emojiNameRe.MatchString(name) {
			t.Errorf("emoji name %q was accepted", name)
		}
	}
}

func TestAssetModeSecurityModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		if assetModesEnforceAccess() {
			t.Fatal("POSIX asset modes must not claim to enforce Windows DACLs")
		}
		if len(AssetStorageSecurityWarnings()) < 2 {
			t.Fatal("Windows startup must surface ACL and power-loss limitations")
		}
		t.Log("Windows assets rely on a restricted inheritable FileRoot ACL; os.Chmod mode bits do not rewrite the DACL")
		return
	}
	if !assetModesEnforceAccess() {
		t.Fatal("POSIX asset mode hardening unexpectedly disabled")
	}
	if warnings := AssetStorageSecurityWarnings(); len(warnings) != 0 {
		t.Fatalf("unexpected POSIX asset warnings: %v", warnings)
	}
}

func TestAssetImageValidationRejectsAnimationAndBoundsDecodes(t *testing.T) {
	if err := validateAssetImage(tinyPNG, ".png"); err != nil {
		t.Fatalf("static PNG rejected: %v", err)
	}
	for _, chunkType := range []string{"acTL", "fcTL", "fdAT"} {
		t.Run("animated PNG "+chunkType, func(t *testing.T) {
			raw := testPNGWithChunk(t, chunkType, nil)
			if err := validateAssetImage(raw, ".png"); err == nil || !strings.Contains(err.Error(), "animated PNG") {
				t.Fatalf("animated PNG error = %v, want explicit rejection", err)
			}
		})
	}
	malformedPNG := append([]byte(nil), tinyPNG...)
	binary.BigEndian.PutUint32(malformedPNG[8:12], ^uint32(0))
	if err := validateStaticImageContainer(malformedPNG); err == nil || !strings.Contains(err.Error(), "malformed PNG") {
		t.Fatalf("oversized PNG chunk error = %v, want safe malformed-container rejection", err)
	}

	if err := validateAssetImage(tinyGIF, ".gif"); err != nil {
		t.Fatalf("static GIF rejected: %v", err)
	}
	if err := validateAssetImage(testAnimatedGIF(t), ".gif"); err == nil || !strings.Contains(err.Error(), "animated GIF") {
		t.Fatalf("animated GIF error = %v, want explicit rejection", err)
	}
	animatedWebPHeader := make([]byte, 30)
	copy(animatedWebPHeader[:4], "RIFF")
	binary.LittleEndian.PutUint32(animatedWebPHeader[4:8], uint32(len(animatedWebPHeader)-8))
	copy(animatedWebPHeader[8:12], "WEBP")
	copy(animatedWebPHeader[12:16], "VP8X")
	binary.LittleEndian.PutUint32(animatedWebPHeader[16:20], 10)
	animatedWebPHeader[20] = 0x02
	if err := validateStaticImageContainer(animatedWebPHeader); err == nil || !strings.Contains(err.Error(), "animated WebP") {
		t.Fatalf("animated WebP error = %v, want explicit rejection", err)
	}
	if got := cap(assetImageDecodeSlots); got != maxConcurrentImageDecodes {
		t.Fatalf("production decode slots = %d, want %d", got, maxConcurrentImageDecodes)
	}

	slots := make(chan struct{}, 1)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := withAssetImageDecodeSlot(slots, func() (assetImageFormat, error) {
			close(firstEntered)
			<-releaseFirst
			return assetImageFormat{}, nil
		})
		firstDone <- err
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		_, err := withAssetImageDecodeSlot(slots, func() (assetImageFormat, error) {
			close(secondEntered)
			return assetImageFormat{}, nil
		})
		secondDone <- err
	}()
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second decode entered while the only slot was occupied")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first decode slot: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second decode slot: %v", err)
	}
}

func TestAssetStorageMigratesSafeLegacyAvatar(t *testing.T) {
	rootDir := t.TempDir()
	avatarDir := filepath.Join(rootDir, "avatars")
	if err := os.Mkdir(avatarDir, 0o777); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"user-uid.png": tinyPNG, "user-uid.jpg": []byte("duplicate")} {
		if err := os.WriteFile(filepath.Join(avatarDir, name), data, 0o666); err != nil {
			t.Fatal(err)
		}
	}

	storage := assetStorage{rootDir: rootDir}
	raw, image, err := storage.readAvatar("user-uid")
	if err != nil {
		t.Fatalf("readAvatar: %v", err)
	}
	if !bytes.Equal(raw, tinyPNG) || image.fileName != avatarAssetBase("user-uid")+".png" {
		t.Fatalf("migrated avatar = %q from %q", raw, image.fileName)
	}
	for _, legacy := range []string{"user-uid.png", "user-uid.jpg"} {
		if _, err := os.Stat(filepath.Join(avatarDir, legacy)); !os.IsNotExist(err) {
			t.Fatalf("legacy avatar %q remains: %v", legacy, err)
		}
	}
	migrated := filepath.Join(avatarDir, image.fileName)
	if got, err := os.ReadFile(migrated); err != nil || !bytes.Equal(got, tinyPNG) {
		t.Fatalf("migrated file = %q, err = %v", got, err)
	}
	if assetModesEnforceAccess() {
		for path, want := range map[string]os.FileMode{
			rootDir:   assetDirMode,
			avatarDir: assetDirMode,
			migrated:  assetFileMode,
		} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != want {
				t.Fatalf("%s mode = %o, want %o", path, got, want)
			}
		}
	}
}

func TestAssetStorageDoesNotFallbackUnsafeLegacyAvatar(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, "avatars"), 0o700); err != nil {
		t.Fatal(err)
	}
	outsideLegacy := filepath.Join(rootDir, "legacy.png")
	if err := os.WriteFile(outsideLegacy, []byte("must-not-read"), 0o600); err != nil {
		t.Fatal(err)
	}

	storage := assetStorage{rootDir: rootDir}
	if _, _, err := storage.readAvatar("../legacy"); !os.IsNotExist(err) {
		t.Fatalf("unsafe legacy lookup error = %v, want not found", err)
	}
	if got, err := os.ReadFile(outsideLegacy); err != nil || string(got) != "must-not-read" {
		t.Fatalf("unsafe legacy target = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "avatars", avatarAssetBase("../legacy")+".png")); !os.IsNotExist(err) {
		t.Fatalf("unsafe legacy avatar was migrated: %v", err)
	}
}

func TestAssetStorageAvatarMigrationSerializesConcurrentSet(t *testing.T) {
	rootDir := t.TempDir()
	avatarDir := filepath.Join(rootDir, "avatars")
	if err := os.Mkdir(avatarDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(avatarDir, "user-uid.png"), tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	storage := assetStorage{rootDir: rootDir}
	legacyRead := make(chan struct{})
	setterAttempted := make(chan struct{})
	allowMigration := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		_, _, err := storage.readAvatarWithHook("user-uid", func() {
			close(legacyRead)
			<-allowMigration
		})
		readDone <- err
	}()
	<-legacyRead
	setDone := make(chan error, 1)
	go func() {
		close(setterAttempted)
		_, err := storage.writeAvatar("user-uid", ".gif", tinyGIF)
		setDone <- err
	}()
	<-setterAttempted
	close(allowMigration)
	if err := <-readDone; err != nil {
		t.Fatalf("migration: %v", err)
	}
	if err := <-setDone; err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, _, err := storage.readAvatar("user-uid")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if !bytes.Equal(raw, tinyGIF) {
		t.Fatalf("concurrent set was overwritten by stale migration: %q", raw)
	}
}

func TestAssetStorageAtomicReplaceAndModes(t *testing.T) {
	rootDir := t.TempDir()
	storage := assetStorage{rootDir: rootDir}

	fileName, err := storage.writeImage("emojis", "party", ".png", tinyPNG)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if fileName != "party.png" {
		t.Fatalf("file name = %q", fileName)
	}
	if _, err := storage.writeImage("emojis", "party", ".png", tinyPNG); err != nil {
		t.Fatalf("same-extension replacement: %v", err)
	}
	if _, err := storage.writeImage("emojis", "party", ".gif", tinyGIF); err != nil {
		t.Fatalf("format replacement: %v", err)
	}

	raw, image, err := storage.readImage("emojis", "party")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(raw, tinyGIF) || image.fileName != "party.gif" {
		t.Fatalf("read = %q from %q", raw, image.fileName)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "emojis", "party.png")); !os.IsNotExist(err) {
		t.Fatalf("superseded extension still exists: %v", err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(rootDir, "emojis", ".*.tmp")); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files = %v, err = %v", leftovers, err)
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Join(rootDir, "emojis"))
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != assetDirMode {
			t.Fatalf("directory mode = %o, want %o", got, assetDirMode)
		}
		fileInfo, err := os.Stat(filepath.Join(rootDir, "emojis", "party.gif"))
		if err != nil {
			t.Fatal(err)
		}
		if got := fileInfo.Mode().Perm(); got != assetFileMode {
			t.Fatalf("file mode = %o, want %o", got, assetFileMode)
		}
	}

	renamed, err := storage.renameImage("emojis", "party", "celebrate")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed != "celebrate.gif" {
		t.Fatalf("renamed file = %q", renamed)
	}
	images, err := storage.listImages("emojis")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(images) != 1 || images[0].base != "celebrate" {
		t.Fatalf("images = %+v", images)
	}
	if _, err := storage.removeImage("emojis", "celebrate"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, _, err := storage.readImage("emojis", "celebrate"); !os.IsNotExist(err) {
		t.Fatalf("removed image read error = %v", err)
	}
}

func TestReadRegularAssetRejectsIdentitySwap(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "image.png"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "replacement.png"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	opener := func(name string) (*os.File, error) {
		if err := root.Remove(name); err != nil {
			return nil, err
		}
		if err := root.Rename("replacement.png", name); err != nil {
			return nil, err
		}
		return root.Open(name)
	}
	if _, err := readRegularAssetWithOpener(root, "image.png", 1024, opener); !errors.Is(err, errUnsafeAsset) {
		t.Fatalf("identity swap error = %v, want errUnsafeAsset", err)
	}
}

func TestAssetStorageOpenRootKeepsVerifiedHandle(t *testing.T) {
	parent := t.TempDir()
	rootDir := filepath.Join(parent, "assets")
	if err := os.Mkdir(rootDir, assetDirMode); err != nil {
		t.Fatal(err)
	}
	storage := assetStorage{rootDir: rootDir}

	t.Run("opens pathname once", func(t *testing.T) {
		calls := 0
		root, err := storage.openRootWithOpener(func(path string) (*os.Root, error) {
			calls++
			return os.OpenRoot(path)
		})
		if err != nil {
			t.Fatalf("openRootWithOpener: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("root pathname opened %d times, want exactly once", calls)
		}
	})

	t.Run("path swap after open cannot redirect handle", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows does not permit renaming this opened directory handle")
		}
		moved := filepath.Join(parent, "verified-root")
		calls := 0
		root, err := storage.openRootWithOpener(func(path string) (*os.Root, error) {
			calls++
			opened, err := os.OpenRoot(path)
			if err != nil {
				return nil, err
			}
			if err := os.Rename(path, moved); err != nil {
				_ = opened.Close()
				return nil, err
			}
			if err := os.Mkdir(path, assetDirMode); err != nil {
				_ = opened.Close()
				return nil, err
			}
			return opened, nil
		})
		if err != nil {
			t.Fatalf("open verified root across swap: %v", err)
		}
		defer func() { _ = root.Close() }()
		if calls != 1 {
			t.Fatalf("root pathname opened %d times, want exactly once", calls)
		}
		if _, err := writeImageAtRoot(root, "icons", "1", ".png", tinyPNG); err != nil {
			t.Fatalf("write through verified handle: %v", err)
		}
		if _, err := os.Stat(filepath.Join(moved, "icons", "1.png")); err != nil {
			t.Fatalf("verified directory did not receive write: %v", err)
		}
		if _, err := os.Stat(filepath.Join(rootDir, "icons", "1.png")); !os.IsNotExist(err) {
			t.Fatalf("replacement pathname received redirected write: %v", err)
		}
	})
}

func TestAssetStorageNormalizesExistingModes(t *testing.T) {
	rootDir := t.TempDir()
	assetDir := filepath.Join(rootDir, "emojis")
	if err := os.Mkdir(assetDir, 0o777); err != nil {
		t.Fatal(err)
	}
	assetPath := filepath.Join(assetDir, "party.png")
	if err := os.WriteFile(assetPath, tinyPNG, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rootDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(assetDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(assetPath, 0o666); err != nil {
		t.Fatal(err)
	}

	storage := assetStorage{rootDir: rootDir}
	if _, _, err := storage.readImage("emojis", "party"); err != nil {
		t.Fatalf("read existing asset: %v", err)
	}
	if !assetModesEnforceAccess() {
		return
	}
	for path, want := range map[string]os.FileMode{
		rootDir:   assetDirMode,
		assetDir:  assetDirMode,
		assetPath: assetFileMode,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestAssetStorageCollapsesMultiExtensionOperations(t *testing.T) {
	rootDir := t.TempDir()
	dir := filepath.Join(rootDir, "emojis")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"party.jpg": testTinyJPEG(t), "party.png": tinyPNG, "other.gif": tinyGIF,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	storage := assetStorage{rootDir: rootDir}
	images, err := storage.listImages("emojis")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("logical image count = %d, want 2: %+v", len(images), images)
	}
	var party assetImage
	for _, image := range images {
		if image.base == "party" {
			party = image
		}
	}
	if party.fileName != "party.png" {
		t.Fatalf("chosen party variant = %q, want deterministic PNG", party.fileName)
	}

	renamed, err := storage.renameImage("emojis", "party", "celebrate")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed != "celebrate.png" {
		t.Fatalf("renamed = %q", renamed)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "party.*")); err != nil || len(matches) != 0 {
		t.Fatalf("old variants after rename = %v, err = %v", matches, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "celebrate.jpg"), []byte("duplicate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := storage.removeImage("emojis", "celebrate"); err != nil || removed != "celebrate.png" {
		t.Fatalf("remove = %q, err = %v", removed, err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "celebrate.*")); err != nil || len(matches) != 0 {
		t.Fatalf("variants after delete = %v, err = %v", matches, err)
	}
}

func TestAssetStorageSkipsInvalidPreferredCandidates(t *testing.T) {
	rootDir := t.TempDir()
	dir := filepath.Join(rootDir, "emojis")
	if err := os.Mkdir(dir, assetDirMode); err != nil {
		t.Fatal(err)
	}
	jpegData := testTinyJPEG(t)
	oversized := make([]byte, maxImageBytes+1)
	copy(oversized, tinyPNG)
	files := map[string][]byte{
		"party.png":    []byte("corrupt preferred candidate"),
		"party.jpg":    jpegData,
		"fallback.png": oversized,
		"fallback.gif": tinyGIF,
		"broken.png":   tinyPNG[:24],
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, assetFileMode); err != nil {
			t.Fatal(err)
		}
	}

	storage := assetStorage{rootDir: rootDir}
	raw, selected, err := storage.readImage("emojis", "party")
	if err != nil || selected.fileName != "party.jpg" || !bytes.Equal(raw, jpegData) {
		t.Fatalf("party fallback = %q (%d bytes), err = %v", selected.fileName, len(raw), err)
	}
	images, err := storage.listImages("emojis")
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	got := make([]string, 0, len(images))
	for _, image := range images {
		got = append(got, image.fileName)
	}
	if fmt.Sprint(got) != "[fallback.gif party.jpg]" {
		t.Fatalf("valid listed candidates = %v, want [fallback.gif party.jpg]", got)
	}

	renamed, err := storage.renameImage("emojis", "party", "celebrate")
	if err != nil || renamed != "celebrate.jpg" {
		t.Fatalf("rename valid fallback = %q, err = %v", renamed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "party.png")); !os.IsNotExist(err) {
		t.Fatalf("corrupt source variant survived rename: %v", err)
	}
	raw, selected, err = storage.readImage("emojis", "celebrate")
	if err != nil || selected.fileName != "celebrate.jpg" || !bytes.Equal(raw, jpegData) {
		t.Fatalf("renamed fallback = %q (%d bytes), err = %v", selected.fileName, len(raw), err)
	}
}

func TestDiscoverChannelIconIDsFiltersAndNormalizes(t *testing.T) {
	rootDir := t.TempDir()
	dir := filepath.Join(rootDir, "icons")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"1.png":   tinyPNG,
		"1.jpg":   tinyPNG, // decoded PNG does not match the extension
		"2.gif":   tinyGIF,
		"03.png":  tinyPNG,
		"-4.gif":  tinyGIF,
		"bad.png": tinyPNG,
		"5.PNG":   tinyPNG,
		"6.txt":   []byte("unsupported"),
		"9.png":   tinyPNG[:24], // sniffable header, incomplete image
	}
	oversized := make([]byte, maxImageBytes+1)
	copy(oversized, tinyPNG)
	files["10.png"] = oversized
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "7.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o644); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(outside, filepath.Join(dir, "8.png")) == nil

	ids, err := DiscoverChannelIconIDs(rootDir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("IDs = %v, want [1 2]", ids)
	}
	if assetModesEnforceAccess() {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != assetDirMode {
			t.Fatalf("icons directory mode = %o", got)
		}
		for _, name := range []string{"1.png", "1.jpg", "2.gif"} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != assetFileMode {
				t.Fatalf("%s mode = %o", name, got)
			}
		}
		if linked {
			info, err := os.Stat(outside)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Fatalf("symlink target mode changed to %o", got)
			}
		}
	}
}

func TestAssetStorageRejectsInvalidServedImages(t *testing.T) {
	rootDir := t.TempDir()
	dir := filepath.Join(rootDir, "icons")
	if err := os.Mkdir(dir, assetDirMode); err != nil {
		t.Fatal(err)
	}
	oversized := make([]byte, maxImageBytes+1)
	copy(oversized, tinyPNG)
	files := map[string][]byte{
		"1.png": tinyPNG,
		"2.png": tinyPNG[:24],
		"3.jpg": tinyPNG,
		"4.png": oversized,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, assetFileMode); err != nil {
			t.Fatal(err)
		}
	}
	storage := assetStorage{rootDir: rootDir}
	if raw, _, err := storage.readImage("icons", "1"); err != nil || !bytes.Equal(raw, tinyPNG) {
		t.Fatalf("valid served image = %d bytes, err = %v", len(raw), err)
	}
	for _, base := range []string{"2", "3", "4"} {
		if _, _, err := storage.readImage("icons", base); err == nil {
			t.Errorf("invalid served image %s was accepted", base)
		}
	}
}

func TestAssetStorageRejectsTraversalComponents(t *testing.T) {
	storage := assetStorage{rootDir: t.TempDir()}
	for _, base := range []string{"../escape", `..\escape`, "/absolute", `C:\absolute`, "a/b", `a\b`, ".", ".."} {
		if _, err := storage.writeImage("avatars", base, ".png", []byte("x")); !errors.Is(err, errUnsafeAsset) {
			t.Errorf("writeImage base %q error = %v, want errUnsafeAsset", base, err)
		}
	}
	for _, dir := range []string{"../avatars", `..\avatars`, "/avatars", `C:\avatars`, "nested/avatars"} {
		if _, err := storage.writeImage(dir, "safe", ".png", []byte("x")); !errors.Is(err, errUnsafeAsset) {
			t.Errorf("writeImage dir %q error = %v, want errUnsafeAsset", dir, err)
		}
	}
}

func TestAssetStorageRejectsSymlinkEscape(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.png")
	if err := os.WriteFile(outsidePath, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outsidePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootDir, "avatars"), assetDirMode); err != nil {
		t.Fatal(err)
	}
	base := avatarAssetBase("victim")
	linkPath := filepath.Join(rootDir, "avatars", base+".png")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink creation unavailable on %s: %v", runtime.GOOS, err)
	}

	storage := assetStorage{rootDir: rootDir}
	if _, _, err := storage.readImage("avatars", base); !errors.Is(err, errUnsafeAsset) {
		t.Fatalf("symlink read error = %v, want errUnsafeAsset", err)
	}
	if _, err := storage.writeImage("avatars", base, ".png", []byte("overwrite")); !errors.Is(err, errUnsafeAsset) {
		t.Fatalf("symlink write error = %v, want errUnsafeAsset", err)
	}
	outside, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(outside) != "outside" {
		t.Fatalf("outside file changed to %q", outside)
	}
	if assetModesEnforceAccess() {
		info, err := os.Stat(outsidePath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("symlink target mode changed to %o", got)
		}
	}
}

func TestAssetStorageRejectsNonRegularFile(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, "icons"), assetDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootDir, "icons", "1.png"), assetDirMode); err != nil {
		t.Fatal(err)
	}
	storage := assetStorage{rootDir: rootDir}
	if _, _, err := storage.readImage("icons", "1"); !errors.Is(err, errUnsafeAsset) {
		t.Fatalf("directory read error = %v, want errUnsafeAsset", err)
	}
	if _, err := storage.writeImage("icons", "1", ".png", []byte("x")); !errors.Is(err, errUnsafeAsset) {
		t.Fatalf("directory write error = %v, want errUnsafeAsset", err)
	}
}

func TestAssetStorageConcurrentFormatReplace(t *testing.T) {
	storage := assetStorage{rootDir: t.TempDir()}
	const writers = 24
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		extension := ".png"
		if i%2 != 0 {
			extension = ".gif"
		}
		wg.Add(1)
		go func(index int, ext string) {
			defer wg.Done()
			<-start
			data := tinyPNG
			if ext == ".gif" {
				data = tinyGIF
			}
			_, err := storage.writeImage("emojis", "party", ext, data)
			errs <- err
		}(i, extension)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	images, err := storage.listImages("emojis")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(images) != 1 || images[0].base != "party" {
		t.Fatalf("images after concurrent writes = %+v", images)
	}
	raw, _, err := storage.readImage("emojis", "party")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(raw, tinyPNG) && !bytes.Equal(raw, tinyGIF) {
		t.Fatalf("stored data is not one of the complete test images")
	}
}

func TestGroupIconWriteFailuresRestoreFileAndMetadata(t *testing.T) {
	tests := []struct {
		name string
		ops  assetMutationOps
	}{
		{
			name: "superseded variant removal",
			ops: assetMutationOps{
				remove: func(_ *os.Root, rel string) error {
					if strings.HasSuffix(rel, ".png") {
						return errors.New("injected remove failure")
					}
					return nil
				},
				syncDir: syncAssetDir,
			},
		},
		{
			name: "post-rename directory sync",
			ops: assetMutationOps{
				remove: func(root *os.Root, rel string) error { return root.Remove(rel) },
				syncDir: func(*os.Root, string) error {
					return errors.New("injected directory sync failure")
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := assetStorage{rootDir: t.TempDir()}
			groups := newFakeGroups()
			groupID, err := groups.CreateGroup(context.Background(), "server", "fault", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.writeGroupIconWithMetadata(
				context.Background(), groupID, ".png", tinyPNG, groups,
			); err != nil {
				t.Fatalf("initial group icon: %v", err)
			}
			if _, err := storage.writeGroupIconWithMetadataWithOps(
				context.Background(), groupID, ".gif", tinyGIF, groups, test.ops,
			); err == nil {
				t.Fatal("injected filesystem failure returned nil")
			}
			group, err := groups.GetGroup(context.Background(), "server", groupID)
			if err != nil || group == nil || group.Icon != fmt.Sprintf("%d.png", groupID) {
				t.Fatalf("metadata after filesystem rollback = %+v, err = %v", group, err)
			}
			raw, image, err := storage.readImage("group_icons", strconv.FormatInt(groupID, 10))
			if err != nil || image.fileName != group.Icon || !bytes.Equal(raw, tinyPNG) {
				t.Fatalf("file after filesystem rollback = %q (%d bytes), err = %v", image.fileName, len(raw), err)
			}
			if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(strconv.FormatInt(groupID, 10)))); !os.IsNotExist(err) {
				t.Fatalf("completed rollback left a journal: %v", err)
			}
		})
	}
}

func TestGroupIconMetadataErrorOutcome(t *testing.T) {
	t.Run("applied then error retains journal for commit recovery", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "indeterminate commit", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".png", tinyPNG, groups,
		); err != nil {
			t.Fatalf("initial group icon: %v", err)
		}

		base := strconv.FormatInt(groupID, 10)
		target := base + ".gif"
		groups.mu.Lock()
		groups.setGroupIconHook = func(id int64, icon string) error {
			if id != groupID || icon != target {
				return nil
			}
			groups.mu.Lock()
			groups.groups["server"][id].Icon = icon
			groups.mu.Unlock()
			return errors.New("injected post-commit response failure")
		}
		groups.mu.Unlock()

		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".gif", tinyGIF, groups,
		); err == nil {
			t.Fatal("post-commit response failure returned nil")
		}
		group, err := groups.GetGroup(context.Background(), "server", groupID)
		if err != nil || group == nil || group.Icon != target {
			t.Fatalf("committed metadata = %+v, err = %v", group, err)
		}
		raw, image, err := storage.readImage("group_icons", base)
		if err != nil || image.fileName != target || !bytes.Equal(raw, tinyGIF) {
			t.Fatalf("retained target = %q (%d bytes), err = %v", image.fileName, len(raw), err)
		}
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		journal, journalErr := readGroupIconJournal(root, base)
		closeErr := root.Close()
		if journalErr != nil || closeErr != nil {
			t.Fatalf("retained journal: read=%v close=%v", journalErr, closeErr)
		}
		if journal.PriorIcon != base+".png" || journal.TargetFile != target {
			t.Fatalf("journal outcome = prior %q target %q", journal.PriorIcon, journal.TargetFile)
		}

		groups.mu.Lock()
		groups.setGroupIconHook = nil
		groups.mu.Unlock()
		if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err != nil {
			t.Fatalf("recover committed outcome: %v", err)
		}
		group, _ = groups.GetGroup(context.Background(), "server", groupID)
		raw, image, err = storage.readImage("group_icons", base)
		if err != nil || group == nil || group.Icon != target || image.fileName != target || !bytes.Equal(raw, tinyGIF) {
			t.Fatalf("recovered commit: group=%+v image=%q bytes=%d err=%v", group, image.fileName, len(raw), err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("commit recovery left journal: %v", err)
		}
	})

	t.Run("definite pre-commit failure rolls back and cleans journal", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "definite rollback", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".png", tinyPNG, groups,
		); err != nil {
			t.Fatalf("initial group icon: %v", err)
		}
		base := strconv.FormatInt(groupID, 10)
		groups.mu.Lock()
		groups.setGroupIconHook = func(_ int64, icon string) error {
			if icon == base+".gif" {
				return errors.New("injected pre-commit failure")
			}
			return nil
		}
		groups.mu.Unlock()

		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".gif", tinyGIF, groups,
		); err == nil {
			t.Fatal("pre-commit failure returned nil")
		}
		group, err := groups.GetGroup(context.Background(), "server", groupID)
		raw, image, readErr := storage.readImage("group_icons", base)
		if err != nil || readErr != nil || group == nil || group.Icon != base+".png" ||
			image.fileName != group.Icon || !bytes.Equal(raw, tinyPNG) {
			t.Fatalf("rolled-back outcome: group=%+v image=%q bytes=%d get=%v read=%v", group, image.fileName, len(raw), err, readErr)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("definite rollback left journal: %v", err)
		}
	})
}

func TestServerGroupDeleteWithAssetsOutcome(t *testing.T) {
	t.Run("confirmed delete cleans orphan variants and journal", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete cleanup", 0)
		if err != nil {
			t.Fatal(err)
		}
		base := strconv.FormatInt(groupID, 10)
		if _, err := storage.writeImage("group_icons", base, ".png", tinyPNG); err != nil {
			t.Fatalf("write orphan PNG: %v", err)
		}
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeAssetAtomic(root, filepath.Join("group_icons", base+".gif"), tinyGIF); err != nil {
			t.Fatalf("write orphan GIF: %v", err)
		}
		if err := writeGroupIconJournal(root, newGroupIconJournal(groupID, "", base+".png", nil)); err != nil {
			t.Fatalf("write pending journal: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}

		deleted, err := storage.deleteServerGroupWithAssets(context.Background(), groupID, false, groups)
		if err != nil || !deleted {
			t.Fatalf("delete outcome = deleted %t, err %v", deleted, err)
		}
		group, err := groups.GetGroup(context.Background(), "server", groupID)
		if err != nil || group != nil {
			t.Fatalf("group after delete = %+v, err = %v", group, err)
		}
		for _, rel := range []string{
			filepath.Join("group_icons", base+".png"),
			filepath.Join("group_icons", base+".gif"),
			groupIconJournalPath(base),
			groupIconDeleteTombstonePath(base),
		} {
			if _, err := os.Stat(filepath.Join(storage.rootDir, rel)); !os.IsNotExist(err) {
				t.Errorf("deleted group artifact %q remains: %v", rel, err)
			}
		}
	})

	t.Run("post-commit error is confirmed before cleanup", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete committed", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".png", tinyPNG, groups,
		); err != nil {
			t.Fatalf("write group icon: %v", err)
		}
		base := strconv.FormatInt(groupID, 10)
		groups.mu.Lock()
		groups.deleteGroupHook = func(groupType string, id int64, _ bool) error {
			groups.mu.Lock()
			delete(groups.groups[groupType], id)
			groups.mu.Unlock()
			return errors.New("injected post-commit delete response failure")
		}
		groups.mu.Unlock()

		deleted, err := storage.deleteServerGroupWithAssets(context.Background(), groupID, false, groups)
		if err != nil || !deleted {
			t.Fatalf("confirmed post-commit outcome = deleted %t, err %v", deleted, err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, "group_icons", base+".png")); !os.IsNotExist(err) {
			t.Fatalf("confirmed deleted group's icon remains: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); !os.IsNotExist(err) {
			t.Fatalf("confirmed delete left tombstone: %v", err)
		}
	})

	t.Run("indeterminate delete retains recoverable state", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete indeterminate", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".png", tinyPNG, groups,
		); err != nil {
			t.Fatalf("write group icon: %v", err)
		}
		base := strconv.FormatInt(groupID, 10)
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		prior, err := snapshotImageVariants(root, "group_icons", base)
		if err != nil {
			t.Fatalf("snapshot prior icon: %v", err)
		}
		if err := writeGroupIconJournal(root, newGroupIconJournal(groupID, base+".png", base+".gif", prior)); err != nil {
			t.Fatalf("write pending journal: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		groups.mu.Lock()
		groups.deleteGroupHook = func(string, int64, bool) error {
			groups.mu.Lock()
			groups.getGroupErr = errors.New("injected confirmation lookup failure")
			groups.mu.Unlock()
			return errors.New("injected indeterminate delete failure")
		}
		groups.mu.Unlock()

		deleted, err := storage.deleteServerGroupWithAssets(context.Background(), groupID, false, groups)
		if deleted || !errors.Is(err, errGroupDeleteIndeterminate) {
			t.Fatalf("ambiguous outcome = deleted %t, err %v", deleted, err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, "group_icons", base+".png")); err != nil {
			t.Fatalf("ambiguous delete removed icon: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); err != nil {
			t.Fatalf("ambiguous delete removed journal: %v", err)
		}
		root, err = storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		tombstone, tombstoneErr := readGroupIconDeleteTombstone(root, base)
		closeErr := root.Close()
		if tombstoneErr != nil || closeErr != nil || tombstone.GroupID != groupID {
			t.Fatalf("retained delete tombstone = %+v, read=%v close=%v", tombstone, tombstoneErr, closeErr)
		}

		groups.mu.Lock()
		groups.getGroupErr = nil
		groups.deleteGroupHook = nil
		groups.mu.Unlock()
		group, err := groups.GetGroup(context.Background(), "server", groupID)
		if err != nil || group == nil {
			t.Fatalf("group was not retained after pre-commit ambiguity: %+v, err = %v", group, err)
		}
		deleted, err = storage.deleteServerGroupWithAssets(context.Background(), groupID, false, groups)
		if err != nil || !deleted {
			t.Fatalf("retry delete outcome = deleted %t, err %v", deleted, err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, "group_icons", base+".png")); !os.IsNotExist(err) {
			t.Fatalf("retry left icon: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("retry left journal: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); !os.IsNotExist(err) {
			t.Fatalf("retry left delete tombstone: %v", err)
		}
	})
}

func TestServerGroupIconSetDeleteSerialize(t *testing.T) {
	t.Run("set first then delete removes committed icon", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "set first", 0)
		if err != nil {
			t.Fatal(err)
		}
		base := strconv.FormatInt(groupID, 10)
		setEntered := make(chan struct{})
		releaseSet := make(chan struct{})
		var setOnce sync.Once
		groups.mu.Lock()
		groups.setGroupIconHook = func(id int64, icon string) error {
			if id == groupID && icon == base+".png" {
				setOnce.Do(func() {
					close(setEntered)
					<-releaseSet
				})
			}
			return nil
		}
		groups.mu.Unlock()
		setDone := make(chan error, 1)
		go func() {
			_, err := storage.writeGroupIconWithMetadata(context.Background(), groupID, ".png", tinyPNG, groups)
			setDone <- err
		}()
		<-setEntered

		deleteEntered := make(chan struct{})
		var deleteOnce sync.Once
		groups.mu.Lock()
		groups.deleteGroupHook = func(string, int64, bool) error {
			deleteOnce.Do(func() { close(deleteEntered) })
			return nil
		}
		groups.mu.Unlock()
		type deleteResult struct {
			deleted bool
			err     error
		}
		deleteDone := make(chan deleteResult, 1)
		go func() {
			deleted, err := storage.deleteServerGroupWithAssets(context.Background(), groupID, false, groups)
			deleteDone <- deleteResult{deleted: deleted, err: err}
		}()
		select {
		case <-deleteEntered:
			close(releaseSet)
			t.Fatal("delete reached the database while the icon transaction held its lock")
		case <-time.After(50 * time.Millisecond):
		}

		close(releaseSet)
		if err := <-setDone; err != nil {
			t.Fatalf("icon set: %v", err)
		}
		result := <-deleteDone
		if result.err != nil || !result.deleted {
			t.Fatalf("delete outcome = deleted %t, err %v", result.deleted, result.err)
		}
		if _, _, err := storage.readImage("group_icons", base); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("set-first delete left readable icon: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("set-first delete left journal: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); !os.IsNotExist(err) {
			t.Fatalf("set-first delete left tombstone: %v", err)
		}
	})

	t.Run("delete first makes later set fail without orphan", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete first", 0)
		if err != nil {
			t.Fatal(err)
		}
		base := strconv.FormatInt(groupID, 10)
		deleteEntered := make(chan struct{})
		releaseDelete := make(chan struct{})
		var deleteOnce sync.Once
		groups.mu.Lock()
		groups.deleteGroupHook = func(string, int64, bool) error {
			deleteOnce.Do(func() {
				close(deleteEntered)
				<-releaseDelete
			})
			return nil
		}
		groups.mu.Unlock()
		type deleteResult struct {
			deleted bool
			err     error
		}
		deleteDone := make(chan deleteResult, 1)
		go func() {
			deleted, err := storage.deleteServerGroupWithAssets(context.Background(), groupID, false, groups)
			deleteDone <- deleteResult{deleted: deleted, err: err}
		}()
		<-deleteEntered

		setDone := make(chan error, 1)
		go func() {
			_, err := storage.writeGroupIconWithMetadata(context.Background(), groupID, ".png", tinyPNG, groups)
			setDone <- err
		}()
		select {
		case err := <-setDone:
			close(releaseDelete)
			t.Fatalf("icon set did not wait for deletion; returned %v", err)
		case <-time.After(50 * time.Millisecond):
		}

		close(releaseDelete)
		result := <-deleteDone
		if result.err != nil || !result.deleted {
			t.Fatalf("delete outcome = deleted %t, err %v", result.deleted, result.err)
		}
		if err := <-setDone; !errors.Is(err, errAssetGroupMissing) {
			t.Fatalf("post-delete icon set error = %v, want missing group", err)
		}
		if _, _, err := storage.readImage("group_icons", base); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("delete-first race left readable icon: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("delete-first race left journal: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); !os.IsNotExist(err) {
			t.Fatalf("delete-first race left tombstone: %v", err)
		}
	})
}

func TestGroupIconWriteDoubleRollbackFailureRetainsJournal(t *testing.T) {
	storage := assetStorage{rootDir: t.TempDir()}
	groups := newFakeGroups()
	groupID, err := groups.CreateGroup(context.Background(), "server", "double rollback", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.writeGroupIconWithMetadata(
		context.Background(), groupID, ".png", tinyPNG, groups,
	); err != nil {
		t.Fatalf("initial group icon: %v", err)
	}

	restoreCalls := 0
	ops := assetMutationOps{
		remove: func(root *os.Root, rel string) error {
			if strings.HasSuffix(rel, ".png") {
				return errors.New("injected mutation failure")
			}
			return root.Remove(rel)
		},
		syncDir: syncAssetDir,
		restore: func(*os.Root, string, string, []assetImageSnapshot) error {
			restoreCalls++
			return errors.New("injected rollback failure")
		},
	}
	if _, err := storage.writeGroupIconWithMetadataWithOps(
		context.Background(), groupID, ".gif", tinyGIF, groups, ops,
	); err == nil {
		t.Fatal("double rollback failure returned nil")
	}
	if restoreCalls != 2 {
		t.Fatalf("restore attempts = %d, want inner and outer attempts", restoreCalls)
	}
	base := strconv.FormatInt(groupID, 10)
	journalPath := filepath.Join(storage.rootDir, groupIconJournalPath(base))
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("journal was not retained after failed outer rollback: %v", err)
	}
	group, err := groups.GetGroup(context.Background(), "server", groupID)
	if err != nil || group == nil || group.Icon != base+".png" {
		t.Fatalf("metadata after double rollback failure = %+v, err = %v", group, err)
	}

	if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err != nil {
		t.Fatalf("startup recovery after retained journal: %v", err)
	}
	raw, image, err := storage.readImage("group_icons", base)
	if err != nil || image.fileName != base+".png" || !bytes.Equal(raw, tinyPNG) {
		t.Fatalf("recovered pre-image = %q (%d bytes), err = %v", image.fileName, len(raw), err)
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("successful recovery left journal: %v", err)
	}
}

func TestGroupIconUploadRepairsDatabaseDiskDrift(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, assetStorage, *fakeGroups, int64)
	}{
		{
			name: "empty metadata with orphan file",
			setup: func(t *testing.T, storage assetStorage, _ *fakeGroups, groupID int64) {
				t.Helper()
				base := strconv.FormatInt(groupID, 10)
				if _, err := storage.writeImage("group_icons", base, ".png", tinyPNG); err != nil {
					t.Fatalf("write orphan: %v", err)
				}
			},
		},
		{
			name: "metadata points to missing file",
			setup: func(t *testing.T, _ assetStorage, groups *fakeGroups, groupID int64) {
				t.Helper()
				if err := groups.SetGroupIcon(context.Background(), groupID, fmt.Sprintf("%d.png", groupID)); err != nil {
					t.Fatalf("set stale metadata: %v", err)
				}
			},
		},
		{
			name: "metadata points to corrupt file",
			setup: func(t *testing.T, storage assetStorage, groups *fakeGroups, groupID int64) {
				t.Helper()
				base := strconv.FormatInt(groupID, 10)
				if _, err := storage.writeImage("group_icons", base, ".png", []byte("not an image")); err != nil {
					t.Fatalf("write corrupt icon: %v", err)
				}
				if err := groups.SetGroupIcon(context.Background(), groupID, base+".png"); err != nil {
					t.Fatalf("set corrupt metadata: %v", err)
				}
			},
		},
		{
			name: "metadata points to oversized file",
			setup: func(t *testing.T, storage assetStorage, groups *fakeGroups, groupID int64) {
				t.Helper()
				base := strconv.FormatInt(groupID, 10)
				if _, err := storage.writeImage("group_icons", base, ".png", make([]byte, maxImageBytes+1)); err != nil {
					t.Fatalf("write oversized icon: %v", err)
				}
				if err := groups.SetGroupIcon(context.Background(), groupID, base+".png"); err != nil {
					t.Fatalf("set oversized metadata: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := assetStorage{rootDir: t.TempDir()}
			groups := newFakeGroups()
			groupID, err := groups.CreateGroup(context.Background(), "server", test.name, 0)
			if err != nil {
				t.Fatalf("CreateGroup: %v", err)
			}
			test.setup(t, storage, groups, groupID)

			fileName, err := storage.writeGroupIconWithMetadata(
				context.Background(), groupID, ".gif", tinyGIF, groups,
			)
			if err != nil {
				t.Fatalf("repairing upload: %v", err)
			}
			base := strconv.FormatInt(groupID, 10)
			if want := base + ".gif"; fileName != want {
				t.Fatalf("file name = %q, want %q", fileName, want)
			}
			group, err := groups.GetGroup(context.Background(), "server", groupID)
			if err != nil || group == nil || group.Icon != fileName {
				t.Fatalf("group metadata = %+v, err = %v", group, err)
			}
			raw, image, err := storage.readImage("group_icons", base)
			if err != nil || image.fileName != fileName || !bytes.Equal(raw, tinyGIF) {
				t.Fatalf("repaired image = %q (%d bytes), err = %v", image.fileName, len(raw), err)
			}
			root, err := storage.openRoot()
			if err != nil {
				t.Fatalf("open root: %v", err)
			}
			variants, variantsErr := existingImageVariants(root, "group_icons", base)
			closeErr := root.Close()
			if variantsErr != nil || closeErr != nil {
				t.Fatalf("inspect repaired variants: variants=%v close=%v", variantsErr, closeErr)
			}
			if len(variants) != 1 || variants[0].fileName != fileName {
				t.Fatalf("repaired variants = %+v, want only %q", variants, fileName)
			}
			if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
				t.Fatalf("successful repair left a journal: %v", err)
			}
		})
	}
}

func TestGroupIconJournalFailsClosedOnRecomputedInconsistency(t *testing.T) {
	base := "7"
	journal := newGroupIconJournal(7, base+".png", base+".gif", []assetImageSnapshot{{
		image: assetImage{
			base:        base,
			fileName:    base + ".png",
			contentType: "image/png",
			extension:   ".png",
		},
		data: tinyPNG,
	}})
	if err := validateGroupIconJournal(journal); err != nil {
		t.Fatalf("valid journal rejected: %v", err)
	}
	clone := func() groupIconJournal {
		cloned := journal
		cloned.PriorImages = append([]groupIconPriorImage(nil), journal.PriorImages...)
		for index := range cloned.PriorImages {
			cloned.PriorImages[index].Data = append([]byte(nil), cloned.PriorImages[index].Data...)
		}
		return cloned
	}

	t.Run("whole journal checksum", func(t *testing.T) {
		tampered := clone()
		tampered.TargetFile = base + ".png"
		if err := validateGroupIconJournal(tampered); err == nil || !strings.Contains(err.Error(), "journal checksum") {
			t.Fatalf("tampered checksum error = %v", err)
		}
	})

	t.Run("recomputed metadata mismatch", func(t *testing.T) {
		tampered := clone()
		tampered.PriorIcon = base + ".jpg"
		tampered.Checksum = groupIconJournalChecksum(tampered)
		if err := validateGroupIconJournal(tampered); err == nil || !strings.Contains(err.Error(), "no matching snapshot") {
			t.Fatalf("recomputed metadata mismatch error = %v", err)
		}
	})

	t.Run("recomputed image format mismatch", func(t *testing.T) {
		tampered := clone()
		tampered.PriorImages[0].Data = append([]byte(nil), tinyGIF...)
		sum := sha256.Sum256(tampered.PriorImages[0].Data)
		tampered.PriorImages[0].SHA256 = hex.EncodeToString(sum[:])
		tampered.Checksum = groupIconJournalChecksum(tampered)
		if err := validateGroupIconJournal(tampered); err == nil || !strings.Contains(err.Error(), "does not match .png") {
			t.Fatalf("recomputed image mismatch error = %v", err)
		}
	})

	t.Run("recomputed multi-variant baseline", func(t *testing.T) {
		tampered := clone()
		sum := sha256.Sum256(tinyGIF)
		tampered.PriorImages = append(tampered.PriorImages, groupIconPriorImage{
			Extension: ".gif",
			Data:      append([]byte(nil), tinyGIF...),
			SHA256:    hex.EncodeToString(sum[:]),
		})
		tampered.Checksum = groupIconJournalChecksum(tampered)
		if err := validateGroupIconJournal(tampered); err == nil || !strings.Contains(err.Error(), "exactly one canonical") {
			t.Fatalf("multi-variant baseline error = %v", err)
		}
	})

	t.Run("snapshots without prior metadata", func(t *testing.T) {
		tampered := clone()
		tampered.PriorIcon = ""
		tampered.Checksum = groupIconJournalChecksum(tampered)
		if err := validateGroupIconJournal(tampered); err == nil || !strings.Contains(err.Error(), "without prior metadata") {
			t.Fatalf("orphan snapshot error = %v", err)
		}
	})
}

func TestRecoverGroupIconTransactions(t *testing.T) {
	t.Run("rolls back file when metadata did not commit", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, _ := groups.CreateGroup(context.Background(), "server", "rollback crash", 0)
		if _, err := storage.writeGroupIconWithMetadata(context.Background(), groupID, ".png", tinyPNG, groups); err != nil {
			t.Fatal(err)
		}
		base := strconv.FormatInt(groupID, 10)
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		prior, err := snapshotImageVariants(root, "group_icons", base)
		if err != nil {
			t.Fatal(err)
		}
		group, _ := groups.GetGroup(context.Background(), "server", groupID)
		if err := writeGroupIconJournal(root, newGroupIconJournal(groupID, group.Icon, base+".gif", prior)); err != nil {
			t.Fatal(err)
		}
		if _, err := writeImageAtRoot(root, "group_icons", base, ".gif", tinyGIF); err != nil {
			t.Fatal(err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}

		if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err != nil {
			t.Fatalf("recover: %v", err)
		}
		group, _ = groups.GetGroup(context.Background(), "server", groupID)
		raw, image, err := storage.readImage("group_icons", base)
		if err != nil || group.Icon != base+".png" || image.fileName != group.Icon || !bytes.Equal(raw, tinyPNG) {
			t.Fatalf("recovered rollback: group=%+v image=%q bytes=%d err=%v", group, image.fileName, len(raw), err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("rollback journal remains: %v", err)
		}
	})

	t.Run("finalizes file when metadata committed", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, _ := groups.CreateGroup(context.Background(), "server", "commit crash", 0)
		base := strconv.FormatInt(groupID, 10)
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeGroupIconJournal(root, newGroupIconJournal(groupID, "", base+".png", nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := writeImageAtRoot(root, "group_icons", base, ".png", tinyPNG); err != nil {
			t.Fatal(err)
		}
		if err := groups.SetGroupIcon(context.Background(), groupID, base+".png"); err != nil {
			t.Fatal(err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}

		if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err != nil {
			t.Fatalf("recover: %v", err)
		}
		group, _ := groups.GetGroup(context.Background(), "server", groupID)
		raw, image, err := storage.readImage("group_icons", base)
		if err != nil || group.Icon != base+".png" || image.fileName != group.Icon || !bytes.Equal(raw, tinyPNG) {
			t.Fatalf("recovered commit: group=%+v image=%q bytes=%d err=%v", group, image.fileName, len(raw), err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconJournalPath(base))); !os.IsNotExist(err) {
			t.Fatalf("commit journal remains: %v", err)
		}
	})
}

func TestRecoverGroupIconDeleteTombstones(t *testing.T) {
	t.Run("finishes cleanup after database commit", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete replay", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".png", tinyPNG, groups,
		); err != nil {
			t.Fatalf("write group icon: %v", err)
		}
		base := strconv.FormatInt(groupID, 10)
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeGroupIconDeleteTombstone(root, newGroupIconDeleteTombstone(groupID)); err != nil {
			t.Fatalf("write delete tombstone: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		if err := groups.DeleteGroup(context.Background(), "server", groupID, false); err != nil {
			t.Fatalf("commit database deletion: %v", err)
		}

		if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err != nil {
			t.Fatalf("recover committed deletion: %v", err)
		}
		if _, _, err := storage.readImage("group_icons", base); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("replayed delete left icon: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); !os.IsNotExist(err) {
			t.Fatalf("replayed delete left tombstone: %v", err)
		}
	})

	t.Run("cancels tombstone when database delete did not commit", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete canceled", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".gif", tinyGIF, groups,
		); err != nil {
			t.Fatalf("write group icon: %v", err)
		}
		base := strconv.FormatInt(groupID, 10)
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeGroupIconDeleteTombstone(root, newGroupIconDeleteTombstone(groupID)); err != nil {
			t.Fatalf("write delete tombstone: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}

		if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err != nil {
			t.Fatalf("recover canceled deletion: %v", err)
		}
		group, err := groups.GetGroup(context.Background(), "server", groupID)
		raw, image, readErr := storage.readImage("group_icons", base)
		if err != nil || readErr != nil || group == nil || group.Icon != base+".gif" ||
			image.fileName != group.Icon || !bytes.Equal(raw, tinyGIF) {
			t.Fatalf("canceled delete state: group=%+v image=%q bytes=%d get=%v read=%v", group, image.fileName, len(raw), err, readErr)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); !os.IsNotExist(err) {
			t.Fatalf("canceled delete left tombstone: %v", err)
		}
	})

	t.Run("tampered tombstone fails closed", func(t *testing.T) {
		storage := assetStorage{rootDir: t.TempDir()}
		groups := newFakeGroups()
		groupID, err := groups.CreateGroup(context.Background(), "server", "delete tampered", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.writeGroupIconWithMetadata(
			context.Background(), groupID, ".png", tinyPNG, groups,
		); err != nil {
			t.Fatalf("write group icon: %v", err)
		}
		base := strconv.FormatInt(groupID, 10)
		tombstone := newGroupIconDeleteTombstone(groupID)
		tombstone.Checksum = strings.Repeat("0", sha256.Size*2)
		raw, err := json.Marshal(tombstone)
		if err != nil {
			t.Fatal(err)
		}
		root, err := storage.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeAssetAtomic(root, groupIconDeleteTombstonePath(base), raw); err != nil {
			t.Fatalf("write tampered tombstone: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}

		if err := RecoverGroupIconTransactions(context.Background(), storage.rootDir, groups); err == nil ||
			!strings.Contains(err.Error(), "tombstone checksum") {
			t.Fatalf("tampered tombstone recovery error = %v", err)
		}
		if _, _, err := storage.readImage("group_icons", base); err != nil {
			t.Fatalf("fail-closed recovery mutated live icon: %v", err)
		}
		if _, err := os.Stat(filepath.Join(storage.rootDir, groupIconDeleteTombstonePath(base))); err != nil {
			t.Fatalf("fail-closed recovery removed tombstone: %v", err)
		}
	})
}

func TestAssetStorageLogicalLocksDoNotBlockUnrelatedAssets(t *testing.T) {
	storage := assetStorage{rootDir: t.TempDir()}
	groups := newFakeGroups()
	groupID, _ := groups.CreateGroup(context.Background(), "server", "slow metadata", 0)
	if _, err := storage.writeAvatar("reader", ".png", tinyPNG); err != nil {
		t.Fatal(err)
	}

	metadataEntered := make(chan struct{})
	releaseMetadata := make(chan struct{})
	var first sync.Once
	groups.mu.Lock()
	groups.setGroupIconHook = func(_ int64, icon string) error {
		if strings.HasSuffix(icon, ".png") {
			first.Do(func() {
				close(metadataEntered)
				<-releaseMetadata
			})
		}
		return nil
	}
	groups.mu.Unlock()

	firstDone := make(chan error, 1)
	go func() {
		_, err := storage.writeGroupIconWithMetadata(context.Background(), groupID, ".png", tinyPNG, groups)
		firstDone <- err
	}()
	<-metadataEntered

	sameGroupDone := make(chan error, 1)
	go func() {
		_, err := storage.writeGroupIconWithMetadata(context.Background(), groupID, ".gif", tinyGIF, groups)
		sameGroupDone <- err
	}()
	unrelatedDone := make(chan error, 1)
	go func() {
		if _, _, err := storage.readAvatar("reader"); err != nil {
			unrelatedDone <- err
			return
		}
		_, err := storage.writeImage("emojis", "party", ".png", tinyPNG)
		unrelatedDone <- err
	}()
	select {
	case err := <-unrelatedDone:
		if err != nil {
			t.Fatalf("unrelated asset operation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated assets were blocked by slow group metadata")
	}
	select {
	case err := <-sameGroupDone:
		t.Fatalf("same-group update did not serialize; completed early with %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseMetadata)
	if err := <-firstDone; err != nil {
		t.Fatalf("first group update: %v", err)
	}
	if err := <-sameGroupDone; err != nil {
		t.Fatalf("serialized group update: %v", err)
	}
	group, _ := groups.GetGroup(context.Background(), "server", groupID)
	if group == nil || group.Icon != fmt.Sprintf("%d.gif", groupID) {
		t.Fatalf("final serialized metadata = %+v", group)
	}
}

func TestAssetStorageLogicalLockEntriesAreEvictedAfterChurn(t *testing.T) {
	storage := assetStorage{rootDir: t.TempDir()}
	locks := storage.lockSet()
	const requests = 512

	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for index := 0; index < requests; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, _, err := storage.readImage("emojis", fmt.Sprintf("missing-%d", index))
			if !errors.Is(err, os.ErrNotExist) {
				errs <- err
			}
		}(index)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("missing asset read: %v", err)
	}

	locks.namespaces.mu.Lock()
	namespaceEntries := len(locks.namespaces.entries)
	locks.namespaces.mu.Unlock()
	locks.bases.mu.Lock()
	baseEntries := len(locks.bases.entries)
	locks.bases.mu.Unlock()
	if namespaceEntries != 0 || baseEntries != 0 {
		t.Fatalf("logical lock registry retained namespace=%d base=%d entries", namespaceEntries, baseEntries)
	}
}

func TestAssetStorageLogicalLockWaitersKeepOneIdentity(t *testing.T) {
	storage := assetStorage{rootDir: t.TempDir()}
	locks := storage.lockSet()
	const key = "emojis\x00shared"

	entryWithRefs := func(want int) *assetLockEntry {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			locks.bases.mu.Lock()
			entry := locks.bases.entries[key]
			refs := 0
			if entry != nil {
				refs = entry.refs
			}
			locks.bases.mu.Unlock()
			if refs == want {
				return entry
			}
			if time.Now().After(deadline) {
				t.Fatalf("base lock refs = %d, want %d", refs, want)
			}
			runtime.Gosched()
		}
	}

	unlockFirst := storage.lockAssets("emojis", true, "shared")
	firstEntry := entryWithRefs(1)
	secondAcquired := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		unlock := storage.lockAssets("emojis", true, "shared")
		close(secondAcquired)
		<-releaseSecond
		unlock()
		close(secondDone)
	}()
	if entry := entryWithRefs(2); entry != firstEntry {
		t.Fatal("waiter registered under a different logical lock identity")
	}
	select {
	case <-secondAcquired:
		unlockFirst()
		close(releaseSecond)
		<-secondDone
		t.Fatal("second writer acquired while the first writer held the lock")
	case <-time.After(50 * time.Millisecond):
	}

	unlockFirst()
	select {
	case <-secondAcquired:
	case <-time.After(3 * time.Second):
		t.Fatal("second writer did not acquire after first release")
	}
	if entry := entryWithRefs(1); entry != firstEntry {
		t.Fatal("lock identity changed while the second writer held it")
	}

	thirdAcquired := make(chan struct{})
	releaseThird := make(chan struct{})
	thirdDone := make(chan struct{})
	go func() {
		unlock := storage.lockAssets("emojis", true, "shared")
		close(thirdAcquired)
		<-releaseThird
		unlock()
		close(thirdDone)
	}()
	if entry := entryWithRefs(2); entry != firstEntry {
		t.Fatal("later waiter registered under a different logical lock identity")
	}
	select {
	case <-thirdAcquired:
		close(releaseSecond)
		close(releaseThird)
		t.Fatal("third writer acquired while the second writer held the lock")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseSecond)
	<-secondDone
	select {
	case <-thirdAcquired:
	case <-time.After(3 * time.Second):
		t.Fatal("third writer did not acquire after second release")
	}
	close(releaseThird)
	<-thirdDone

	locks.namespaces.mu.Lock()
	namespaceEntries := len(locks.namespaces.entries)
	locks.namespaces.mu.Unlock()
	locks.bases.mu.Lock()
	baseEntries := len(locks.bases.entries)
	locks.bases.mu.Unlock()
	if namespaceEntries != 0 || baseEntries != 0 {
		t.Fatalf("logical lock registry retained namespace=%d base=%d entries", namespaceEntries, baseEntries)
	}
}
