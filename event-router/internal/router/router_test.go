package router

import (
	"encoding/json"
	"testing"

	"eventrouter/internal/event"
	"eventrouter/internal/sender"
	"eventrouter/internal/store"
)

func boolPtr(b bool) *bool { return &b }

func testStore() *store.Store {
	return store.New(
		[]store.Project{
			{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: boolPtr(true)},
			{ID: 2, AppID: "app.two", Enabled: false, ConsentCheckEnabled: boolPtr(true)},
		},
		[]store.Mapping{
			{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"},
		},
		[]store.Consent{
			{ProjectID: 1, UserID: "alice", Approved: true},
			{ProjectID: 1, UserID: "bob", Approved: true},
		},
	)
}

func payment(project int, user string) event.Event {
	return eventFixture(project, user, event.TypePayment, 0)
}

func refund(project int, user string) event.Event {
	return eventFixture(project, user, event.TypeRefund, 0)
}

func eventFixture(project int, user string, typ event.Type, dryRun int) event.Event {
	return event.Event{
		ID: "ev-1", ProjectID: project, UserID: user, Type: typ, DryRun: dryRun,
		Data: event.Billing{InvoiceID: 1, Amount: 9.99, Currency: "USD"},
	}
}

func TestProcess(t *testing.T) {
	created := payment(1, "alice")
	created.Type = event.TypeCreated

	tests := []struct {
		name string
		ev   event.Event
		want Status
	}{
		{"payment sent", payment(1, "alice"), StatusSent},
		{"refund sent", refund(1, "alice"), StatusSent},
		{"no mapping", payment(1, "bob"), StatusNoMapping},
		{"refund no mapping", refund(1, "bob"), StatusNoMapping},
		{"disabled project", payment(2, "alice"), StatusProjectDisabled},
		{"unknown project", payment(99, "alice"), StatusValidationFailed},
		{"unsupported type", created, StatusValidationFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(testStore(), &sender.Fake{})
			if got := r.Process(tt.ev); got.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Reason, tt.want)
			}
		})
	}
}

func TestProcess_Payload(t *testing.T) {
	fake := &sender.Fake{}
	New(testStore(), fake).Process(payment(1, "alice"))

	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d payloads, want 1", len(sent))
	}
	if p := sent[0]; p.EventName != "purchase" || p.Amount != 9.99 || p.MMPID != "mmp-alice" || p.Sandbox {
		t.Fatalf("payload = %+v", p)
	}
}

func TestProcess_RefundPayload(t *testing.T) {
	fake := &sender.Fake{}
	New(testStore(), fake).Process(refund(1, "alice"))

	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d payloads, want 1", len(sent))
	}
	if p := sent[0]; p.EventName != "refund" || p.Amount != 9.99 || p.MMPID != "mmp-alice" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestProcess_RefundAmountMustBePositive(t *testing.T) {
	ev := refund(1, "alice")
	ev.Data.Amount = 0
	fake := &sender.Fake{}
	got := New(testStore(), fake).Process(ev)
	if got.Status != StatusValidationFailed {
		t.Fatalf("status = %s (%s), want VALIDATION_FAILED", got.Status, got.Reason)
	}
	if len(fake.Sent()) != 0 {
		t.Fatalf("sent %d payloads, want 0", len(fake.Sent()))
	}
}

func TestMMPEventName(t *testing.T) {
	purchase, err := mmpEventName(event.TypePayment)
	if err != nil || purchase != "purchase" {
		t.Fatalf("payment = %q, err = %v", purchase, err)
	}
	refundName, err := mmpEventName(event.TypeRefund)
	if err != nil || refundName != "refund" {
		t.Fatalf("refund = %q, err = %v", refundName, err)
	}
	if name, err := mmpEventName(event.TypeCreated); err == nil {
		t.Fatalf("created mapped to %q, want error", name)
	}
}

func TestProcess_TemporaryErrorIsRetried(t *testing.T) {
	r := New(testStore(), &sender.Fake{Err: sender.ErrTemporary})
	if got := r.Process(payment(1, "alice")); got.Status != StatusRetry {
		t.Fatalf("status = %s, want RETRY", got.Status)
	}
}

func TestProcess_Consent(t *testing.T) {
	enabled := boolPtr(true)
	disabled := boolPtr(false)

	tests := []struct {
		name     string
		project  store.Project
		mappings []store.Mapping
		consents []store.Consent
		ev       event.Event
		want     Status
		sent     int
	}{
		{
			name:     "explicit refusal",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: false}},
			ev:       payment(1, "alice"),
			want:     StatusGDPRNotApproved,
		},
		{
			name:     "refusal wins over missing mapping",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: false}},
			ev:       payment(1, "alice"),
			want:     StatusGDPRNotApproved,
		},
		{
			name:     "refund refusal",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: false}},
			ev:       refund(1, "alice"),
			want:     StatusGDPRNotApproved,
		},
		{
			name:     "consent not received yet",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			ev:       payment(1, "alice"),
			want:     StatusRetry,
		},
		{
			name:    "missing consent wins over missing mapping",
			project: store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			ev:      payment(1, "alice"),
			want:    StatusRetry,
		},
		{
			name:     "null flag checks consent",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: false}},
			ev:       payment(1, "alice"),
			want:     StatusGDPRNotApproved,
		},
		{
			name:     "partner collects consent",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: disabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: false}},
			ev:       payment(1, "alice"),
			want:     StatusSent,
			sent:     1,
		},
		{
			name:     "partner collects consent without our record",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: disabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			ev:       payment(1, "alice"),
			want:     StatusSent,
			sent:     1,
		},
		{
			name:     "user agreed",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: true}},
			ev:       payment(1, "alice"),
			want:     StatusSent,
			sent:     1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &sender.Fake{}
			st := store.New([]store.Project{tt.project}, tt.mappings, tt.consents)
			got := New(st, fake).Process(tt.ev)
			if got.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Reason, tt.want)
			}
			if len(fake.Sent()) != tt.sent {
				t.Fatalf("sent %d payloads, want %d", len(fake.Sent()), tt.sent)
			}
		})
	}
}

func TestProcess_DryRun(t *testing.T) {
	enabled := boolPtr(true)
	disabled := boolPtr(false)
	const dryPayment = `{
		"id": "ev-1",
		"project_id": 1,
		"user_id": "alice",
		"type": "billing-payment",
		"dry_run": 1,
		"data": {"invoice_id": 1, "amount": 9.99, "currency": "USD"}
	}`
	const dryRefund = `{
		"id": "ev-1",
		"project_id": 1,
		"user_id": "alice",
		"type": "billing-refund",
		"dry_run": 1,
		"data": {"invoice_id": 1, "amount": 9.99, "currency": "USD"}
	}`

	tests := []struct {
		name     string
		project  store.Project
		mappings []store.Mapping
		consents []store.Consent
		raw      string
		want     Status
		sandbox  bool
	}{
		{
			name:     "sandbox when consent approved",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: true}},
			raw:      dryPayment,
			want:     StatusSent,
			sandbox:  true,
		},
		{
			name:     "sandbox refund",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: true}},
			raw:      dryRefund,
			want:     StatusSent,
			sandbox:  true,
		},
		{
			name:     "sandbox refusal is not sent",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			consents: []store.Consent{{ProjectID: 1, UserID: "alice", Approved: false}},
			raw:      dryPayment,
			want:     StatusGDPRNotApproved,
		},
		{
			name:     "sandbox waits for consent",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: enabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			raw:      dryPayment,
			want:     StatusRetry,
		},
		{
			name:     "sandbox when partner collects consent",
			project:  store.Project{ID: 1, AppID: "app.one", Enabled: true, ConsentCheckEnabled: disabled},
			mappings: []store.Mapping{{AppID: "app.one", UserID: "alice", MMPID: "mmp-alice"}},
			raw:      dryPayment,
			want:     StatusSent,
			sandbox:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ev event.Event
			if err := json.Unmarshal([]byte(tt.raw), &ev); err != nil {
				t.Fatal(err)
			}
			fake := &sender.Fake{}
			st := store.New([]store.Project{tt.project}, tt.mappings, tt.consents)
			got := New(st, fake).Process(ev)
			if got.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Reason, tt.want)
			}
			if tt.want != StatusSent {
				if len(fake.Sent()) != 0 {
					t.Fatalf("sent %d payloads, want 0", len(fake.Sent()))
				}
				return
			}
			sent := fake.Sent()
			if len(sent) != 1 || sent[0].Sandbox != tt.sandbox {
				t.Fatalf("sent = %+v, want one sandbox=%v", sent, tt.sandbox)
			}
		})
	}
}
