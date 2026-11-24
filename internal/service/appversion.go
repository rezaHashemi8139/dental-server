package service

import (
	"context"

	"connectrpc.com/connect"

	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/releases"
)

// AppVersionHeader carries the desktop app's version ("major.minor.patch") on every call.
const AppVersionHeader = "Dental-App-Version"

// anyVersionProcedures answer every app version: the health check tells an outdated
// app that it must update, and the login screen still offers languages.
var anyVersionProcedures = map[string]bool{
	dentalv1connect.HealthServiceCheckProcedure:           true,
	dentalv1connect.LanguageServiceListLanguagesProcedure: true,
	dentalv1connect.LanguageServiceGetLanguageProcedure:   true,
}

// AppVersionInterceptor refuses calls from desktop apps older than the current
// release's minimum version (a forced update, see internal/releases) with
// FAILED_PRECONDITION "update_required": an outdated app cannot log in or sync
// until it is updated. An app that does not say its version counts as the oldest.
// A nil store forces nothing.
func AppVersionInterceptor(store *releases.Store) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if anyVersionProcedures[req.Spec().Procedure] {
				return next(ctx, req)
			}
			version, _ := releases.ParseVersion(req.Header().Get(AppVersionHeader))
			if store.Current().Requires(version) {
				return nil, fail(connect.CodeFailedPrecondition, reasonUpdateRequired)
			}
			return next(ctx, req)
		}
	}
}
