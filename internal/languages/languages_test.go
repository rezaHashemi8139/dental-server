package languages

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
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

// Persian (fa.json, a copy of the desktop app's built-in src/locales/fa.json) is the
// reference: text missing from a file shows in Persian, so every file must have the
// same keys as it, with the same {{placeholders}}. The desktop app's
// `pnpm sync-locales` brings the files in line.
func TestLanguagesMatchPersian(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	textsOf := func(l Language) map[string]string {
		var tree map[string]any
		if err := json.Unmarshal(l.Content, &tree); err != nil {
			t.Fatal(err)
		}
		texts := map[string]string{}
		var walk func(prefix string, node map[string]any)
		walk = func(prefix string, node map[string]any) {
			for k, v := range node {
				if prefix == "" && k == "_meta" {
					continue
				}
				if child, ok := v.(map[string]any); ok {
					walk(prefix+k+".", child)
				} else {
					texts[prefix+k] = fmt.Sprint(v)
				}
			}
		}
		walk("", tree)
		return texts
	}
	placeholder := regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)
	placeholdersOf := func(text string) string {
		var names []string
		for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
			names = append(names, m[1])
		}
		slices.Sort(names)
		return strings.Join(names, ", ")
	}

	var reference map[string]string
	for _, l := range all {
		if l.Code == "fa" {
			reference = textsOf(l)
		}
	}
	if reference == nil {
		t.Fatal("fa.json is missing")
	}
	for _, l := range all {
		texts := textsOf(l)
		for k, want := range reference {
			got, ok := texts[k]
			if !ok {
				t.Errorf("%s.json: missing %q", l.Code, k)
			} else if placeholdersOf(got) != placeholdersOf(want) {
				t.Errorf("%s.json: %q has placeholders [%s], fa.json has [%s]", l.Code, k, placeholdersOf(got), placeholdersOf(want))
			}
		}
		for k := range texts {
			if _, ok := reference[k]; !ok {
				t.Errorf("%s.json: %q is not in fa.json", l.Code, k)
			}
		}
	}
}

func TestParse(t *testing.T) {
	ok := `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "version": 2, "currency": {"default": "EUR", "minorUnit": true}}, "app": {}}`
	l, err := parse("de", []byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if l.Abbr != "DE" || l.Version != 2 {
		t.Errorf("got %+v", l)
	}

	bad := map[string]string{
		"not json":            `{`,
		"no meta":             `{"app": {}}`,
		"code differs":        `{"_meta": {"code": "fr", "name": "French", "dir": "ltr", "version": 1, "currency": {"default": "EUR", "minorUnit": true}}}`,
		"no name":             `{"_meta": {"code": "de", "name": " ", "dir": "ltr", "version": 1, "currency": {"default": "EUR", "minorUnit": true}}}`,
		"bad dir":             `{"_meta": {"code": "de", "name": "German", "dir": "up", "version": 1, "currency": {"default": "EUR", "minorUnit": true}}}`,
		"no currency":         `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "version": 1}}`,
		"bad currency code":   `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "version": 1, "currency": {"default": "eur", "minorUnit": true}}}`,
		"no default":          `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "version": 1, "currency": {"minorUnit": true}}}`,
		"no minor unit":       `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "version": 1, "currency": {"default": "EUR"}}}`,
		"minor unit not bool": `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "version": 1, "currency": {"default": "EUR", "minorUnit": "yes"}}}`,
		"no version":          `{"_meta": {"code": "de", "name": "German", "dir": "ltr", "currency": {"default": "EUR", "minorUnit": true}}}`,
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
