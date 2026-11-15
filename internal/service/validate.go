package service

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// ValidateInterceptor checks every request against the buf.validate rules written
// in proto/ (lengths, formats, the password policy...). The handlers rely on it and
// do not repeat those checks, so every handler must be served with it.
//
// A failed check returns INVALID_ARGUMENT with a stable reason as the message:
// "weak_password" when the password policy is the only problem, otherwise
// "invalid_argument". The violations (which field, which rule) are attached as an
// error detail of type buf.validate.Violations, useful while developing the client.
func ValidateInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			msg, ok := req.Any().(proto.Message)
			if !ok {
				return next(ctx, req)
			}
			err := protovalidate.Validate(msg)
			if err == nil {
				return next(ctx, req)
			}

			validationErr, ok := errors.AsType[*protovalidate.ValidationError](err)
			if !ok {
				// The rules themselves are broken (CEL compile or runtime error): a server bug.
				return nil, internal(err)
			}
			connectErr := connect.NewError(connect.CodeInvalidArgument, errors.New(reasonFor(validationErr)))
			if detail, detailErr := connect.NewErrorDetail(validationErr.ToProto()); detailErr == nil {
				connectErr.AddDetail(detail)
			}
			return nil, connectErr
		}
	}
}

// reasonFor picks the error reason: weak_password only if every violation is the password policy.
func reasonFor(err *protovalidate.ValidationError) string {
	for _, v := range err.Violations {
		if v.Proto.GetRuleId() != reasonWeakPassword {
			return reasonInvalidArgument
		}
	}
	return reasonWeakPassword
}
