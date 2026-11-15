package service

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
)

func TestStrongPassword(t *testing.T) {
	tests := []struct {
		password string
		want     bool
	}{
		{"Abcdefg1", true},        // exactly 8
		{"Passw0rd-doctor", true}, // symbols are allowed
		{"Abcdef1", false},        // 7 characters
		{"abcdefg1", false},       // no uppercase
		{"ABCDEFG1", false},       // no lowercase
		{"Abcdefgh", false},       // no digit
		{"12345678", false},       // digits only
		{"", false},               // empty
		{"Ab1" + strings.Repeat("x", 5), true},
	}
	for _, tt := range tests {
		if got := strongPassword(tt.password); got != tt.want {
			t.Errorf("strongPassword(%q) = %v, want %v", tt.password, got, tt.want)
		}
	}
}

// SignUp rejects weak passwords with a dedicated reason, before touching the database.
func TestSignUpWeakPassword(t *testing.T) {
	auth := NewAuthService(&Deps{})
	ctx := context.Background()

	_, err := auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "doctor", Password: "password1"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonWeakPassword)

	// An invalid username is still reported as invalid_argument, whatever the password.
	_, err = auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "x", Password: "weak"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	_, err = auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "doctor", Password: "Ab1" + strings.Repeat("x", maxPasswordLength)})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
}
