package releases

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion("1.20.3")
	if err != nil || v != (Version{1, 20, 3}) {
		t.Fatalf("ParseVersion = %v, %v", v, err)
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "1.x.3", "-1.2.3", "v1.2.3"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	if !(Version{0, 9, 9}).Less(Version{0, 10, 0}) || (Version{1, 0, 0}).Less(Version{1, 0, 0}) {
		t.Error("Less compares numbers, not text")
	}
}

// writeRelease puts an installer and release.json into a new folder.
func writeRelease(t *testing.T, release map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.exe"), []byte("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, dir, release)
	return dir
}

func writeJSON(t *testing.T, dir string, release map[string]any) {
	t.Helper()
	content, _ := json.Marshal(release)
	if err := os.WriteFile(filepath.Join(dir, FileName), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func release(version, minimum string) map[string]any {
	return map[string]any{
		"version":        version,
		"minimumVersion": minimum,
		"notes":          "Fixes",
		"pubDate":        "2026-11-24T10:00:00Z",
		"platforms": map[string]any{
			"windows-x86_64": map[string]any{"file": "setup.exe", "signature": "c2lnbmF0dXJl"},
		},
	}
}

func TestOpen(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil || store.Current() != nil {
		t.Fatalf("empty folder: %v, %v", store, err)
	}

	store, err = Open(writeRelease(t, release("0.3.0", "0.2.0")))
	if err != nil {
		t.Fatal(err)
	}
	r := store.Current()
	if r.Version != (Version{0, 3, 0}) || r.MinimumVersion != (Version{0, 2, 0}) {
		t.Fatalf("release = %+v", r)
	}
	if !r.Requires(Version{0, 1, 9}) || r.Requires(Version{0, 2, 0}) || r.Requires(Version{0, 3, 0}) {
		t.Error("Requires: only versions below the minimum must update")
	}

	withPlatform := func(platform map[string]any) map[string]any {
		r := release("0.3.0", "")
		r["platforms"] = map[string]any{"windows-x86_64": platform}
		return r
	}
	broken := map[string]map[string]any{
		"minimum above version": release("0.3.0", "0.4.0"),
		"bad version":           release("0.3", ""),
		"missing installer":     withPlatform(map[string]any{"file": "nope.exe", "signature": "x"}),
		"path in file name":     withPlatform(map[string]any{"file": "../setup.exe", "signature": "x"}),
		"no signature":          withPlatform(map[string]any{"file": "setup.exe"}),
	}
	for name, r := range broken {
		if _, err := Open(writeRelease(t, r)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReloadKeepsLastGoodRelease(t *testing.T) {
	dir := writeRelease(t, release("0.3.0", ""))
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// A new release is picked up without a restart (the size changes, so no clock games are needed).
	writeJSON(t, dir, release("0.10.0", "0.10.0"))
	if got := store.Current().Version; got != (Version{0, 10, 0}) {
		t.Fatalf("after a new release: %v", got)
	}

	// A broken file keeps the last good release: a typo must not lift a forced update.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, FileName), future, future)
	if got := store.Current(); got == nil || got.MinimumVersion != (Version{0, 10, 0}) {
		t.Fatalf("after a broken file: %+v", got)
	}

	// Removing it withdraws the release.
	_ = os.Remove(filepath.Join(dir, FileName))
	if store.Current() != nil {
		t.Fatal("release still offered after release.json was removed")
	}
}

func TestHandler(t *testing.T) {
	store, err := Open(writeRelease(t, release("0.3.0", "")))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "secret.txt"), []byte("not for download"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, store)
	server := httptest.NewServer(mux)
	defer server.Close()

	get := func(path string) *http.Response {
		t.Helper()
		res, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	res := get("/updates/windows/x86_64/0.2.0")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("older app: status %d", res.StatusCode)
	}
	var update updateResponse
	if err := json.NewDecoder(res.Body).Decode(&update); err != nil {
		t.Fatal(err)
	}
	if update.Version != "0.3.0" || update.Signature != "c2lnbmF0dXJl" || update.URL != server.URL+"/updates/files/setup.exe" {
		t.Fatalf("update = %+v", update)
	}

	for _, path := range []string{"/updates/windows/x86_64/0.3.0", "/updates/windows/x86_64/1.0.0", "/updates/darwin/aarch64/0.1.0"} {
		if status := get(path).StatusCode; status != http.StatusNoContent {
			t.Errorf("%s: status %d, want 204", path, status)
		}
	}
	if status := get("/updates/windows/x86_64/latest").StatusCode; status != http.StatusBadRequest {
		t.Errorf("bad version: status %d", status)
	}

	if res := get("/updates/files/setup.exe"); res.StatusCode != http.StatusOK || res.ContentLength != int64(len("installer")) {
		t.Errorf("installer: status %d, length %d", res.StatusCode, res.ContentLength)
	}
	if status := get("/updates/files/secret.txt").StatusCode; status != http.StatusNotFound {
		t.Errorf("file not in the release served: status %d", status)
	}
}
