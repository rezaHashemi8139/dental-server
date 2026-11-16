# Downloadable languages

Interface languages the desktop app can download, served by `LanguageService`
(`proto/dental/v1/language.proto`). Only Persian is built into the app;
every other language, English included, is here. The files are embedded in the server binary, and the server refuses to
start if one is broken.

```
files/<code>.json   one file per language, e.g. de.json
```

The list the app shows comes from each file's own `_meta` section, so there is no
separate index to keep in sync:

```json
"_meta": { "code": "en", "name": "English", "abbr": "EN", "dir": "ltr", "version": 1 }
```

- `code` must equal the file name (`de.json` → `"de"`).
- `dir` is `"ltr"` or `"rtl"`; `abbr` defaults to the code in capitals.
- Raise `version` whenever the file changes: installed copies are updated when it goes up.

## Adding or updating a language

Persian is the reference: `files/fa.json` is a copy of the desktop app's built-in
`src/locales/fa.json`, and every other file must have exactly its keys, with the same
`{{placeholders}}` (text missing from a file shows in Persian). The test checks this.

1. Translate `files/en.json` or the desktop app's `src/locales/fa.json` (its `src/locales/README.md` has the AI prompt).
2. Save it here as `files/<code>.json`, or raise `version` in the file you changed.
3. After Persian changed in the desktop app, run `pnpm sync-locales` there: it copies `fa.json` here,
   puts the keys of every file in Persian's order, removes keys Persian no longer has, raises `version`
   of each changed file, and lists keys that still need a translation (`--check` only reports).
4. Run `go test ./internal/languages/` and deploy the server.
