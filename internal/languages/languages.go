// Package languages holds the interface languages the desktop app can download
// (only Persian is built into it). The files are embedded in the server
// binary; see README.md in this folder for adding one.
package languages

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed files/*.json
var files embed.FS

// Same rule as the desktop app (codes become file names there).
var codePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{1,15}$`)

// Language is one language file and what its `_meta` section says about it.
type Language struct {
	Code    string
	Name    string
	Abbr    string
	Dir     string // "ltr" or "rtl"
	Version uint32
	// The whole file, sent to the app as is.
	Content []byte
}

type meta struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Abbr    string `json:"abbr"`
	Dir     string `json:"dir"`
	Version uint32 `json:"version"`
}

// Load reads and checks every embedded language file, sorted by code.
// A broken file is an error: it should never ship.
func Load() ([]Language, error) {
	return load(files, "files")
}

func load(fsys embed.FS, dir string) ([]Language, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var languages []Language
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		content, err := fsys.ReadFile(path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		language, err := parse(strings.TrimSuffix(name, ".json"), content)
		if err != nil {
			return nil, fmt.Errorf("language file %s: %w", name, err)
		}
		languages = append(languages, language)
	}
	sort.Slice(languages, func(i, j int) bool { return languages[i].Code < languages[j].Code })
	return languages, nil
}

func parse(code string, content []byte) (Language, error) {
	var file struct {
		Meta *meta `json:"_meta"`
	}
	if err := json.Unmarshal(content, &file); err != nil {
		return Language{}, err
	}
	m := file.Meta
	switch {
	case m == nil:
		return Language{}, fmt.Errorf(`missing "_meta" section`)
	case !codePattern.MatchString(code):
		return Language{}, fmt.Errorf("%q is not a valid language code", code)
	case m.Code != code:
		return Language{}, fmt.Errorf(`"_meta.code" is %q, expected %q (the file name)`, m.Code, code)
	case strings.TrimSpace(m.Name) == "":
		return Language{}, fmt.Errorf(`missing "_meta.name"`)
	case m.Dir != "ltr" && m.Dir != "rtl":
		return Language{}, fmt.Errorf(`"_meta.dir" must be "ltr" or "rtl"`)
	case m.Version == 0:
		return Language{}, fmt.Errorf(`"_meta.version" must be 1 or more`)
	}
	abbr := strings.TrimSpace(m.Abbr)
	if abbr == "" {
		abbr = strings.ToUpper(code)
	}
	return Language{Code: code, Name: m.Name, Abbr: abbr, Dir: m.Dir, Version: m.Version, Content: content}, nil
}
