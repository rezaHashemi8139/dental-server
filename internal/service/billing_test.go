package service

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

// rawLogin logs in without opening the keys, to see billing-only logins as they are.
func (e *testEnv) rawLogin(inst *dentalv1.RegisterResponse, username, password string) (*dentalv1.LoginResponse, error) {
	e.t.Helper()
	clientKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	return e.auth.Login(context.Background(), &dentalv1.LoginRequest{
		InstallationId: inst.GetInstallationId(), InstallationSecret: inst.GetInstallationSecret(),
		Username: username, Password: password, ClientPublicKey: clientKey.PublicKey().Bytes(),
	})
}

// billing returns a BillingService client that sends the given session token.
func (e *testEnv) billing(token string) dentalv1connect.BillingServiceClient {
	return dentalv1connect.NewBillingServiceClient(e.server.Client(), e.server.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
}

func TestBuySubscriptionInApp(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	if _, err := AddFirstAdmin(ctx, e.deps.DB, &dentalv1.CreateAdminRequest{
		Username: "root", Password: "Passw0rd-root", FullName: "Root",
	}); err != nil {
		t.Fatal(err)
	}
	session, _, err := e.adminLogin("root", "Passw0rd-root")
	if err != nil {
		t.Fatal(err)
	}
	admin := e.admin(session)
	basic, err := admin.CreatePlan(ctx, &dentalv1.CreatePlanRequest{
		Name: "Basic", MaxClinics: 1, MaxDevicesPerClinic: 1, DurationDays: 30, PriceRials: 30_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	pro, err := admin.CreatePlan(ctx, &dentalv1.CreatePlanRequest{
		Name: "Pro", MaxClinics: 2, MaxDevicesPerClinic: 2, DurationDays: 30, PriceRials: 60_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	basicID, proID := basic.GetPlan().GetId(), pro.GetPlan().GetId()

	// Without a subscription the owner gets a billing-only session: no keys, no offline grant.
	inst := e.register(1, "doctor")
	res, err := e.rawLogin(inst, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetBillingOnly() || res.GetBillingReason() != reasonSubscriptionInactive || !res.GetIsOwner() ||
		res.GetSessionToken() == "" || res.GetDatabaseKeys() != nil || res.GetOfflineGrant() != nil ||
		res.GetSubscriptionEndsAt() != nil {
		t.Fatalf("billing-only login = %v", res)
	}
	token := res.GetSessionToken()
	owner := e.billing(token)

	// It can only buy.
	_, err = e.authWith(token).ConfirmKeyRotation(ctx, &dentalv1.ConfirmKeyRotationRequest{NewVersion: 1})
	wantCode(t, err, connect.CodePermissionDenied, reasonSubscriptionRequired)

	// Other members are refused as before, and cannot buy for the owner.
	e.signUp("nurse")
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO clinic_members (clinic_id, user_id) SELECT c.id, u.id FROM clinics c, users u
		WHERE c.license_number = 'LIC-1' AND u.username = 'nurse'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.rawLogin(inst, "nurse", "Passw0rd-nurse")
	wantCode(t, err, connect.CodePermissionDenied, reasonSubscriptionInactive)

	status, err := owner.GetBillingStatus(ctx, &dentalv1.GetBillingStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetCurrent() != nil || status.GetPaidUntil() != nil || len(status.GetPayTo()) != 0 ||
		len(status.GetClinics()) != 1 || status.GetClinics()[0].GetCovered() || status.GetPendingPayment() != nil {
		t.Fatalf("status without subscription = %v", status)
	}

	plans, err := owner.ListPlans(ctx, &dentalv1.BillingServiceListPlansRequest{})
	if err != nil || len(plans.GetPlans()) != 2 || plans.GetPlans()[0].GetName() != "Basic" {
		t.Fatalf("plans = %v, %v", plans, err)
	}

	// Where to pay is set in the admin panel.
	_, err = admin.CreatePaymentAccount(ctx, &dentalv1.CreatePaymentAccountRequest{BankName: "Bank", HolderName: "Owner"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	account, err := admin.CreatePaymentAccount(ctx, &dentalv1.CreatePaymentAccountRequest{
		BankName: "Bank", HolderName: "Dental Ltd", CardNumber: "6037991234567890",
	})
	if err != nil {
		t.Fatal(err)
	}
	accountID := account.GetAccount().GetId()
	if status, _ = owner.GetBillingStatus(ctx, &dentalv1.GetBillingStatusRequest{}); len(status.GetPayTo()) != 1 {
		t.Fatalf("pay to = %v", status.GetPayTo())
	}

	pay := func(planID []byte, mode dentalv1.PurchaseMode, last4 string) (*dentalv1.Payment, error) {
		res, err := owner.CreatePayment(ctx, &dentalv1.CreatePaymentRequest{
			PlanId: planID, Mode: mode, Method: dentalv1.PaymentMethod_PAYMENT_METHOD_CARD_TO_CARD,
			Details: &dentalv1.CreatePaymentRequest_Transfer{Transfer: &dentalv1.ManualTransfer{
				PaymentAccountId: accountID, Reference: "123456",
				PaidAt: timestamppb.New(time.Now().Add(-time.Hour)), PayerCardLast4: last4,
			}},
		})
		return res.GetPayment(), err
	}
	approve := func(p *dentalv1.Payment) *dentalv1.Subscription {
		t.Helper()
		res, err := admin.ApprovePayment(ctx, &dentalv1.ApprovePaymentRequest{Id: p.GetId(), Note: "statement ok"})
		if err != nil {
			t.Fatal(err)
		}
		if res.GetPayment().GetStatus() != dentalv1.PaymentStatus_PAYMENT_STATUS_APPROVED ||
			string(res.GetPayment().GetSubscriptionId()) != string(res.GetSubscription().GetId()) {
			t.Fatalf("approved = %v", res)
		}
		return res.GetSubscription()
	}

	// Buying the basic plan: quoted, reported, approved.
	quote, err := owner.QuotePayment(ctx, &dentalv1.QuotePaymentRequest{PlanId: basicID, Mode: dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND})
	if err != nil || quote.GetQuote().GetAmountRials() != 30_000_000 || quote.GetQuote().GetCreditRials() != 0 {
		t.Fatalf("quote = %v, %v", quote, err)
	}
	_, err = pay(basicID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, "") // card to card needs the last 4 digits
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	first, err := pay(basicID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, "1234")
	if err != nil || first.GetStatus() != dentalv1.PaymentStatus_PAYMENT_STATUS_PENDING || first.GetAmountRials() != 30_000_000 {
		t.Fatalf("payment = %v, %v", first, err)
	}
	_, err = pay(basicID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, "1234")
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPaymentPending)

	pending, err := admin.ListPayments(ctx, &dentalv1.ListPaymentsRequest{Status: dentalv1.PaymentStatus_PAYMENT_STATUS_PENDING})
	if err != nil || len(pending.GetPayments()) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	if r := pending.GetPayments()[0]; r.GetUser().GetUsername() != "doctor" || r.GetAccount().GetHolderName() != "Dental Ltd" ||
		r.GetIdentity().GetNumber() == "" || r.GetPayment().GetTransfer().GetPayerCardLast4() != "1234" {
		t.Fatalf("review = %v", r)
	}
	approve(first)
	_, err = admin.ApprovePayment(ctx, &dentalv1.ApprovePaymentRequest{Id: first.GetId()})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPaymentNotPending)

	// The billing-only session stays so; logging in again opens the clinic.
	_, err = e.authWith(token).ConfirmKeyRotation(ctx, &dentalv1.ConfirmKeyRotationRequest{NewVersion: 1})
	wantCode(t, err, connect.CodePermissionDenied, reasonSubscriptionRequired)
	if _, err := e.login(inst, "doctor", "Passw0rd-doctor"); err != nil {
		t.Fatal(err)
	}

	// A second computer is beyond the basic plan's device limit: billing-only again, to upgrade.
	second := registerRequest(2, "unused")
	second.Target = &dentalv1.RegisterRequest_ExistingClinic{ExistingClinic: &dentalv1.ExistingClinic{LicenseNumber: "LIC-1"}}
	second.Owner = &dentalv1.RegisterRequest_ExistingOwner{ExistingOwner: &dentalv1.ExistingOwner{
		Username: "doctor", Password: "Passw0rd-doctor",
	}}
	inst2, err := e.installation.Register(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	res, err = e.rawLogin(inst2, "doctor", "Passw0rd-doctor")
	if err != nil || !res.GetBillingOnly() || res.GetBillingReason() != reasonDeviceLimitExceeded {
		t.Fatalf("login beyond the device limit = %v, %v", res, err)
	}

	// Upgrading now: the unused basic time is credited.
	quote, err = owner.QuotePayment(ctx, &dentalv1.QuotePaymentRequest{PlanId: proID, Mode: dentalv1.PurchaseMode_PURCHASE_MODE_UPGRADE_NOW})
	if err != nil {
		t.Fatal(err)
	}
	if q := quote.GetQuote(); q.GetCreditRials() < 29_900_000 || q.GetCreditRials() > 30_000_000 ||
		q.GetAmountRials() != 60_000_000-q.GetCreditRials() || time.Since(q.GetStartsAt().AsTime()) > time.Minute {
		t.Fatalf("upgrade quote = %v", q)
	}
	upgrade, err := pay(proID, dentalv1.PurchaseMode_PURCHASE_MODE_UPGRADE_NOW, "1234")
	if err != nil {
		t.Fatal(err)
	}
	if sub := approve(upgrade); sub.GetPlanName() != "Pro" || time.Since(sub.GetStartsAt().AsTime()) > time.Minute {
		t.Fatalf("upgrade = %v", sub)
	}
	if _, err := e.login(inst2, "doctor", "Passw0rd-doctor"); err != nil {
		t.Fatal(err)
	}

	// Renewing early adds a period after the current one; an upgrade then has to wait.
	renewal, err := pay(basicID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, "1234")
	if err != nil {
		t.Fatal(err)
	}
	approve(renewal)
	status, err = owner.GetBillingStatus(ctx, &dentalv1.GetBillingStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetCurrent().GetPlanName() != "Pro" || len(status.GetUpcoming()) != 1 ||
		!status.GetUpcoming()[0].GetStartsAt().AsTime().Equal(status.GetCurrent().GetEndsAt().AsTime()) ||
		!status.GetPaidUntil().AsTime().Equal(status.GetUpcoming()[0].GetEndsAt().AsTime()) ||
		status.GetCurrent().GetNote() != "" || !status.GetClinics()[0].GetCovered() {
		t.Fatalf("status after renewal = %v", status)
	}
	_, err = owner.QuotePayment(ctx, &dentalv1.QuotePaymentRequest{PlanId: proID, Mode: dentalv1.PurchaseMode_PURCHASE_MODE_UPGRADE_NOW})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonRenewalQueued)

	// A rejected payment shows why; a pending one can be withdrawn.
	rejected, err := pay(basicID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, "1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RejectPayment(ctx, &dentalv1.RejectPaymentRequest{Id: rejected.GetId(), Reason: "No such transfer"}); err != nil {
		t.Fatal(err)
	}
	canceled, err := pay(basicID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, "1234")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := owner.CancelPayment(ctx, &dentalv1.CancelPaymentRequest{Id: canceled.GetId()}); err != nil ||
		res.GetPayment().GetStatus() != dentalv1.PaymentStatus_PAYMENT_STATUS_CANCELED {
		t.Fatalf("cancel = %v, %v", res, err)
	}
	_, err = owner.CancelPayment(ctx, &dentalv1.CancelPaymentRequest{Id: canceled.GetId()})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPaymentNotPending)
	_, err = owner.CancelPayment(ctx, &dentalv1.CancelPaymentRequest{Id: make([]byte, 16)})
	wantCode(t, err, connect.CodeNotFound, reasonPaymentNotFound)
	_, err = admin.RejectPayment(ctx, &dentalv1.RejectPaymentRequest{Id: canceled.GetId(), Reason: "late"})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPaymentNotPending)

	mine, err := owner.ListMyPayments(ctx, &dentalv1.ListMyPaymentsRequest{})
	if err != nil || len(mine.GetPayments()) != 5 {
		t.Fatalf("my payments = %v, %v", mine, err)
	}
	if p := mine.GetPayments()[1]; p.GetStatus() != dentalv1.PaymentStatus_PAYMENT_STATUS_REJECTED || p.GetRejectionReason() != "No such transfer" {
		t.Fatalf("rejected payment = %v", p)
	}

	// Only the owner buys: a member's full session is refused.
	nurse, err := e.login(inst, "nurse", "Passw0rd-nurse")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.billing(nurse.res.GetSessionToken()).ListPlans(ctx, &dentalv1.BillingServiceListPlansRequest{})
	wantCode(t, err, connect.CodePermissionDenied, reasonNotClinicOwner)
}
