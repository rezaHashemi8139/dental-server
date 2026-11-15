package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

func TestPasswordPolicy(t *testing.T) {
	tests := []struct {
		password string
		want     bool
	}{
		{"Abcdefg1", true},                       // exactly 8
		{"Passw0rd-doctor", true},                // symbols are allowed
		{"Ab1" + strings.Repeat("x", 253), true}, // 256, the maximum
		{"Abcdef1", false},                       // 7 characters
		{"abcdefg1", false},                      // no uppercase
		{"ABCDEFG1", false},                      // no lowercase
		{"Abcdefgh", false},                      // no digit
		{"12345678", false},                      // digits only
		{"", false},
		{"Ab1" + strings.Repeat("x", 254), false}, // 257, too long
	}
	for _, tt := range tests {
		err := protovalidate.Validate(&dentalv1.SignUpRequest{Username: "doctor", Password: tt.password})
		if got := err == nil; got != tt.want {
			t.Errorf("password %q: valid = %v, want %v (%v)", tt.password, got, tt.want, err)
		}
	}
}

// Invalid requests are rejected by the interceptor with a stable reason, before
// reaching a handler (the handlers here have no database and would crash).
func TestValidateInterceptor(t *testing.T) {
	interceptors := connect.WithInterceptors(ValidateInterceptor())
	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewAuthServiceHandler(NewAuthService(&Deps{}), interceptors))
	mux.Handle(dentalv1connect.NewInstallationServiceHandler(NewInstallationService(&Deps{}), interceptors))
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := dentalv1connect.NewAuthServiceClient(server.Client(), server.URL)
	installation := dentalv1connect.NewInstallationServiceClient(server.Client(), server.URL)
	ctx := context.Background()

	_, err := auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "doctor", Password: "password1"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonWeakPassword)

	// Another problem besides the password: the generic reason wins.
	_, err = auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "x", Password: "weak"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	_, err = auth.Login(ctx, &dentalv1.LoginRequest{Username: "doctor"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	_, err = installation.Register(ctx, &dentalv1.RegisterRequest{})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	_, err = installation.Register(ctx, &dentalv1.RegisterRequest{Machine: &dentalv1.MachineInfo{
		MachineIdHash: make([]byte, 32),
		Hostname:      strings.Repeat("h", 10_000),
	}})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// The violations travel as an error detail, naming the field.
	connectErr, _ := err.(*connect.Error)
	var fields []string
	for _, d := range connectErr.Details() {
		if v, err := d.Value(); err == nil {
			if violations, ok := v.(*validate.Violations); ok {
				for _, violation := range violations.GetViolations() {
					fields = append(fields, protovalidate.FieldPathString(violation.GetField()))
				}
			}
		}
	}
	if strings.Join(fields, ",") != "machine.hostname" {
		t.Errorf("violation fields = %v, want [machine.hostname]", fields)
	}
}
