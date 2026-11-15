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
"_meta": { "code": "de", "name": "Deutsch", "abbr": "DE", "dir": "ltr", "calendar": "gregory", "version": 1 }
```

- `code` must equal the file name (`de.json` → `"de"`).
- `dir` is `"ltr"` or `"rtl"`; `abbr` defaults to the code in capitals.
- `calendar` is how the app shows dates in this language, as a CLDR / JavaScript `Intl`
  calendar id: `"persian"` (Solar Hijri), `"gregory"` (Gregorian) or
  `"islamic"` (Lunar Hijri). Required. Display only: the app converts dates
  when showing them; the server and the database always store UTC Gregorian time.
- Raise `version` whenever the file changes: installed copies are updated when it goes up.

## Adding or updating a language

1. Translate `files/en.json` or the desktop app's `src/locales/fa.json` (its `src/locales/README.md` has the AI prompt).
   Text missing from a file shows in Persian, so keep every file complete.
2. Save it here as `files/<code>.json`, or raise `version` in the file you changed.
3. Run `go test ./internal/languages/` and deploy the server.
