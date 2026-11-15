package service

import (
	"context"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/languages"
)

// LanguageService serves the downloadable interface languages (internal/languages).
// No database: everything is loaded once at startup.
type LanguageService struct {
	list   []*dentalv1.Language
	byCode map[string]languages.Language
}

var _ dentalv1connect.LanguageServiceHandler = (*LanguageService)(nil)

func NewLanguageService(all []languages.Language) *LanguageService {
	s := &LanguageService{byCode: make(map[string]languages.Language, len(all))}
	for _, l := range all {
		s.list = append(s.list, toProtoLanguage(l))
		s.byCode[l.Code] = l
	}
	return s
}

func (s *LanguageService) ListLanguages(context.Context, *dentalv1.ListLanguagesRequest) (*dentalv1.ListLanguagesResponse, error) {
	return &dentalv1.ListLanguagesResponse{Languages: s.list}, nil
}

func (s *LanguageService) GetLanguage(_ context.Context, req *dentalv1.GetLanguageRequest) (*dentalv1.GetLanguageResponse, error) {
	l, ok := s.byCode[req.GetCode()]
	if !ok {
		return nil, fail(connect.CodeNotFound, reasonLanguageNotFound)
	}
	return &dentalv1.GetLanguageResponse{Language: toProtoLanguage(l), Content: l.Content}, nil
}

func toProtoLanguage(l languages.Language) *dentalv1.Language {
	return &dentalv1.Language{Code: l.Code, Name: l.Name, Abbr: l.Abbr, Dir: l.Dir, Version: l.Version}
}
