package releases

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Paths served by Handler (see Register).
const (
	// GET: what the updater plugin asks; {target}-{arch} is the platform, e.g.
	// "windows" and "x86_64". 204 when there is nothing newer than {current}.
	checkPattern = "GET /updates/{target}/{arch}/{current}"
	// GET: an installer named in release.json.
	filePattern = "GET /updates/files/{name}"
)

// CheckURL is the updater endpoint, as the desktop app writes it (Tauri fills in the variables).
const CheckURL = "/updates/{{target}}/{{arch}}/{{current_version}}"

// Register adds the update endpoints to mux.
func Register(mux *http.ServeMux, s *Store) {
	mux.HandleFunc(checkPattern, s.check)
	mux.HandleFunc(filePattern, s.file)
}

// updateResponse is the updater plugin's "dynamic" format.
type updateResponse struct {
	Version   string `json:"version"`
	Notes     string `json:"notes,omitempty"`
	PubDate   string `json:"pub_date,omitempty"`
	URL       string `json:"url"`
	Signature string `json:"signature"`
}

func (s *Store) check(w http.ResponseWriter, r *http.Request) {
	release := s.Current()
	current, err := ParseVersion(r.PathValue("current"))
	if err != nil {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	if release == nil || !current.Less(release.Version) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	platform, ok := release.Platforms[r.PathValue("target")+"-"+r.PathValue("arch")]
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	res := updateResponse{
		Version:   release.Version.String(),
		Notes:     release.Notes,
		URL:       baseURL(r) + "/updates/files/" + platform.File,
		Signature: platform.Signature,
	}
	if !release.PubDate.IsZero() {
		res.PubDate = release.PubDate.Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}

// file serves only installers of the current release, nothing else in the folder.
func (s *Store) file(w http.ResponseWriter, r *http.Request) {
	release := s.Current()
	name := r.PathValue("name")
	listed := false
	if release != nil {
		for _, p := range release.Platforms {
			listed = listed || p.File == name
		}
	}
	if !listed {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// baseURL is the address the request came to, so download links point back at
// this server. X-Forwarded-Proto is honored for a TLS-terminating proxy in front.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
