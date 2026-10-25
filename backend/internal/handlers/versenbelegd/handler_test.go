package versenbelegd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beepbite/backend/internal/channel"
	intake "github.com/beepbite/backend/internal/channel/versenbelegd"
)

type recordingAccepter struct {
	slug string
	sku  map[string]string
	got  channel.InboundOrder
	res  Result
	err  error
}

func (r *recordingAccepter) Accept(_ context.Context, slug string, order channel.InboundOrder, sku map[string]string) (Result, error) {
	r.slug = slug
	r.sku = sku
	r.got = order
	return r.res, r.err
}

func TestWebhookNotConfigured(t *testing.T) {
	h := &Handler{cfg: Config{}, orders: &recordingAccepter{}, readMap: func(string) (map[string]string, error) {
		t.Fatal("map should not be read")
		return nil, nil
	}}
	rr := post(t, h, `{}`, "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestWebhookSignatureAndParse(t *testing.T) {
	const secret = "trial-secret"
	body := []byte(`{
	  "id": "ord_1",
	  "fulfillment": {"type": "afhaal"},
	  "customer": {"name": "Jan de Vries", "email": "jan@example.nl"},
	  "lines": [{"sku": "broodje-gezond", "quantity": 2, "unit_price_cents": 650}],
	  "payment": {"mode": "betaald", "provider": "mollie", "reference": "tr_abc"}
	}`)
	rec := &recordingAccepter{res: Result{
		OrderID: "uuid", OrderNumber: "VB0001", Status: "confirmed",
		TotalCents: 1300, Currency: "EUR", PaymentMode: "paid", Kitchen: "queued",
	}}
	h := &Handler{
		cfg:    Config{WebhookSecret: secret, LocationSlug: "boslaan"},
		orders: rec,
		readMap: func(string) (map[string]string, error) {
			return map[string]string{"broodje-gezond": "BG"}, nil
		},
	}

	rr := post(t, h, string(body), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: %d %s", rr.Code, rr.Body.String())
	}

	rr = post(t, h, string(body), intake.Sign(secret, body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	if rec.slug != "boslaan" || rec.sku["broodje-gezond"] != "BG" {
		t.Fatalf("slug %q map %v", rec.slug, rec.sku)
	}
	if rec.got.ExternalID != "ord_1" || rec.got.Fulfillment.Type != "collection" || rec.got.Payment.Mode != "paid" {
		t.Fatalf("parsed %+v", rec.got)
	}
	if !strings.Contains(rr.Body.String(), `"order_number":"VB0001"`) {
		t.Fatalf("body %s", rr.Body.String())
	}

	rec.res.Replayed = true
	rr = post(t, h, string(body), intake.Sign(secret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("replay status %d", rr.Code)
	}

	rec.err = &UnknownSKUError{SKUs: []string{"broodje-gezond"}}
	rr = post(t, h, string(body), intake.Sign(secret, body))
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "unknown sku") {
		t.Fatalf("unknown: %d %s", rr.Code, rr.Body.String())
	}
}

func TestWebhookBadJSON(t *testing.T) {
	body := []byte(`{"id":`)
	h := &Handler{
		cfg:     Config{WebhookSecret: "s", LocationSlug: "boslaan"},
		orders:  &recordingAccepter{},
		readMap: func(string) (map[string]string, error) { return nil, nil },
	}
	rr := post(t, h, string(body), intake.Sign("s", body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestDeliveryFeeAndKitchenNote(t *testing.T) {
	loc := location{decimals: 2, deliveryFee: "3.50", freeThreshold: "0.00", timezone: "Europe/Amsterdam"}
	fee, err := deliveryFeeCents(loc, "delivery", 1000)
	if err != nil || fee != 350 {
		t.Fatalf("fee %d err %v", fee, err)
	}
	loc.freeThreshold = "35.00"
	fee, err = deliveryFeeCents(loc, "delivery", 3500)
	if err != nil || fee != 0 {
		t.Fatalf("free fee %d err %v", fee, err)
	}
	fee, err = deliveryFeeCents(loc, "collection", 100)
	if err != nil || fee != 0 {
		t.Fatalf("pickup fee %d err %v", fee, err)
	}

	o, err := intake.Parse([]byte(`{
	  "id": "n1",
	  "fulfillment": {"type": "afhaal", "requested_for": "2026-11-02T11:30:00+01:00"},
	  "customer": {"name": "Jan de Vries", "phone": "0612345678", "company": "School De Rank"},
	  "lines": [{"sku": "s", "quantity": 1}],
	  "payment": {"mode": "pay_on_pickup"},
	  "notes": "zonder mayo"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	note := kitchenNotes(o, zone)
	for _, want := range []string{"Afhalen", "Gewenst: 2026-11-02 11:30", "School De Rank", "zonder mayo", "Nog niet betaald"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q missing %q", note, want)
		}
	}
}

func post(t *testing.T, h *Handler, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/vers-en-belegd/orders", strings.NewReader(body))
	if sig != "" {
		req.Header.Set(intake.SignatureHeader, sig)
	}
	rr := httptest.NewRecorder()
	h.create(rr, req)
	return rr
}
