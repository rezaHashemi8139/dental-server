package service

import (
	"context"
	"math"
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
		err := protovalidate.Validate(signUpRequest(tt.password))
		if got := err == nil; got != tt.want {
			t.Errorf("password %q: valid = %v, want %v (%v)", tt.password, got, tt.want, err)
		}
	}
}

func TestClinicLocation(t *testing.T) {
	tests := []struct {
		location *dentalv1.GeoLocation
		want     bool
	}{
		{&dentalv1.GeoLocation{Latitude: 35.6892, Longitude: 51.3890}, true},
		{&dentalv1.GeoLocation{Latitude: -90, Longitude: 180}, true}, // the limits
		{&dentalv1.GeoLocation{Latitude: 0, Longitude: 51.3890}, true},
		{nil, false},                     // required
		{&dentalv1.GeoLocation{}, false}, // 0, 0: never set
		{&dentalv1.GeoLocation{Latitude: 90.1, Longitude: 51}, false},
		{&dentalv1.GeoLocation{Latitude: 35, Longitude: -180.1}, false},
		{&dentalv1.GeoLocation{Latitude: math.NaN(), Longitude: 51}, false},
	}
	for _, tt := range tests {
		req := registerRequest(1, "doctor")
		req.GetClinic().Location = tt.location
		err := protovalidate.Validate(req)
		if got := err == nil; got != tt.want {
			t.Errorf("location %v: valid = %v, want %v (%v)", tt.location, got, tt.want, err)
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

	_, err := auth.SignUp(ctx, signUpRequest("password1"))
	wantCode(t, err, connect.CodeInvalidArgument, reasonWeakPassword)

	// Another problem besides the password: the generic reason wins.
	_, err = auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "x", Password: "weak"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	_, err = auth.Login(ctx, &dentalv1.LoginRequest{Username: "doctor"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	_, err = installation.Register(ctx, &dentalv1.RegisterRequest{})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// Clinic and owner are required, with their formats; a new owner gets the password policy.
	noOwner := registerRequest(1, "doctor")
	noOwner.Owner = nil
	_, err = installation.Register(ctx, noOwner)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	badClinic := registerRequest(1, "doctor")
	badClinic.GetClinic().LicenseNumber = "x"
	badClinic.GetClinic().Phone = "call me"
	_, err = installation.Register(ctx, badClinic)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	weakOwner := registerRequest(1, "doctor")
	weakOwner.GetNewOwner().Password = "password1"
	_, err = installation.Register(ctx, weakOwner)
	wantCode(t, err, connect.CodeInvalidArgument, reasonWeakPassword)

	longHostname := registerRequest(1, "doctor")
	longHostname.GetMachine().Hostname = strings.Repeat("h", 10_000)
	_, err = installation.Register(ctx, longHostname)
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

// signUpRequest is a valid SignUpRequest except, maybe, for its password.
func signUpRequest(password string) *dentalv1.SignUpRequest {
	return &dentalv1.SignUpRequest{
		Username: "doctor", Password: password, FullName: "Dr. Doctor",
		Identity: nationalIdentity("0013542419"), Phone: "09121234567",
	}
}
