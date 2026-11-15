package languages

import (
	"encoding/json"
	"testing"
)

// Every shipped language file must load (the server refuses to start otherwise).
func TestEmbeddedLanguagesLoad(t *testing.T) {
	languages, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(languages) == 0 {
		t.Fatal("no languages embedded")
	}
	for _, l := range languages {
		if l.Name == "" || l.Version == 0 || len(l.Content) == 0 {
			t.Errorf("%s: incomplete %+v", l.Code, l)
		}
	}
}

// Text missing from a file shows in the app's built-in Persian, so every file must
// have the same keys as the English one.
func TestLanguagesHaveTheSameKeys(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	keysOf := func(l Language) map[string]bool {
		var tree map[string]any
		if err := json.Unmarshal(l.Content, &tree); err != nil {
			t.Fatal(err)
		}
		keys := map[string]bool{}
		var walk func(prefix string, node map[string]any)
		walk = func(prefix string, node map[string]any) {
			for k, v := range node {
				if prefix == "" && k == "_meta" {
					continue
				}
				if child, ok := v.(map[string]any); ok {
					walk(prefix+k+".", child)
				} else {
					keys[prefix+k] = true
				}
			}
		}
		walk("", tree)
		return keys
	}

	var reference map[string]bool
	for _, l := range all {
		if l.Code == "en" {
			reference = keysOf(l)
		}
	}
	if reference == nil {
		t.Fatal("en.json is missing")
	}
	for _, l := range all {
		keys := keysOf(l)
		for k := range reference {
			if !keys[k] {
				t.Errorf("%s.json: missing %q", l.Code, k)
			}
		}
		for k := range keys {
			if !reference[k] {
				t.Errorf("%s.json: %q is not in en.json", l.Code, k)
			}
		}
	}
}

func TestParse(t *testing.T) {
	ok := `{"_meta": {"code": "de", "name": "Deutsch", "dir": "ltr", "version": 2}, "app": {}}`
	l, err := parse("de", []byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if l.Abbr != "DE" || l.Version != 2 {
		t.Errorf("got %+v", l)
	}

	bad := map[string]string{
		"not json":     `{`,
		"no meta":      `{"app": {}}`,
		"code differs": `{"_meta": {"code": "fr", "name": "Français", "dir": "ltr", "version": 1}}`,
		"no name":      `{"_meta": {"code": "de", "name": " ", "dir": "ltr", "version": 1}}`,
		"bad dir":      `{"_meta": {"code": "de", "name": "Deutsch", "dir": "up", "version": 1}}`,
		"no version":   `{"_meta": {"code": "de", "name": "Deutsch", "dir": "ltr"}}`,
	}
	for name, content := range bad {
		if _, err := parse("de", []byte(content)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := parse("../de", []byte(ok)); err == nil {
		t.Error("bad code: expected an error")
	}
}
