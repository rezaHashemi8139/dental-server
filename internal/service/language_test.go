package service

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/internal/languages"
)

func TestLanguageService(t *testing.T) {
	s := NewLanguageService([]languages.Language{
		{Code: "de", Name: "Deutsch", Abbr: "DE", Dir: "ltr", Version: 3, Content: []byte(`{"_meta":{}}`)},
	})
	ctx := context.Background()

	list, err := s.ListLanguages(ctx, &dentalv1.ListLanguagesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetLanguages()) != 1 || list.GetLanguages()[0].GetVersion() != 3 {
		t.Fatalf("list: %v", list.GetLanguages())
	}

	got, err := s.GetLanguage(ctx, &dentalv1.GetLanguageRequest{Code: "de"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetLanguage().GetName() != "Deutsch" || string(got.GetContent()) != `{"_meta":{}}` {
		t.Fatalf("get: %v", got)
	}

	_, err = s.GetLanguage(ctx, &dentalv1.GetLanguageRequest{Code: "fr"})
	if connect.CodeOf(err) != connect.CodeNotFound || err.Error() != "not_found: "+reasonLanguageNotFound {
		t.Fatalf("missing language: %v", err)
	}
}
