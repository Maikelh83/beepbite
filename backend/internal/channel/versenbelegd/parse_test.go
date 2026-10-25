package versenbelegd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beepbite/backend/internal/channel"
)

const sample = `{
  "id": "ord_lunch_1001",
  "placed_at": "2026-10-25T09:12:00+02:00",
  "fulfillment": {
    "type": "bezorg",
    "requested_for": "2026-10-25T11:30:00+02:00",
    "address": {
      "company": "Gemeente Veenendaal",
      "line1": "Raadhuisplein 1",
      "postal_code": "3901 GA",
      "city": "Veenendaal",
      "notes": "Receptie, 2e verdieping"
    }
  },
  "customer": {
    "name": "Jan de Vries",
    "email": "Jan@Example.nl",
    "phone": "+31 6 12345678",
    "company": "Gemeente Veenendaal",
    "cost_center": "HR"
  },
  "lines": [
    {"sku": "broodje-gezond", "name": "Broodje gezond", "quantity": 8, "unit_price_cents": 650, "notes": "zonder ui"},
    {"sku": "koffiekan", "name": "Koffiekan", "quantity": 1, "unit_price_cents": 1250}
  ],
  "payment": {"mode": "betaald", "provider": "mollie", "reference": "tr_test_123"},
  "notes": "Graag voor 11:30"
}`

func TestParseSample(t *testing.T) {
	o, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.ExternalID != "ord_lunch_1001" {
		t.Fatalf("id %q", o.ExternalID)
	}
	if o.Fulfillment.Type != "delivery" {
		t.Fatalf("fulfillment %q", o.Fulfillment.Type)
	}
	if o.Fulfillment.Address.Line1 != "Raadhuisplein 1" {
		t.Fatalf("address %+v", o.Fulfillment.Address)
	}
	wantFor := time.Date(2026, 10, 25, 11, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	if !o.Fulfillment.RequestedFor.Equal(wantFor) {
		t.Fatalf("requested %s", o.Fulfillment.RequestedFor)
	}
	if o.Customer.Email != "jan@example.nl" || o.Customer.Phone != "+31612345678" {
		t.Fatalf("customer %+v", o.Customer)
	}
	if o.Customer.Name != "Jan de Vries" {
		t.Fatalf("name %q", o.Customer.Name)
	}
	if len(o.Lines) != 2 || o.Lines[0].Quantity != 8 || o.Lines[0].UnitPriceCents == nil || *o.Lines[0].UnitPriceCents != 650 {
		t.Fatalf("lines %+v", o.Lines)
	}
	if o.Payment.Mode != "paid" || o.Payment.Provider != "mollie" || o.Payment.Reference != "tr_test_123" {
		t.Fatalf("payment %+v", o.Payment)
	}
	if IdempotencyKey(o.ExternalID) != "vers-en-belegd:ord_lunch_1001" {
		t.Fatalf("key %s", IdempotencyKey(o.ExternalID))
	}
	if (Source{}).Name() != "vers-en-belegd" {
		t.Fatal("name")
	}
}

func TestParseAliases(t *testing.T) {
	body := []byte(`{
	  "id": "afhaal-1",
	  "fulfillment": {"type": "afhaal"},
	  "customer": {"name": "Piet", "phone": "0612345678"},
	  "lines": [{"sku": "frikandel", "quantity": 2}],
	  "payment": {"mode": "op_rekening", "house_account_name": "School De Rank"}
	}`)
	o, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if o.Fulfillment.Type != "collection" || o.Payment.Mode != "on_account" || o.Payment.HouseAccountName != "School De Rank" {
		t.Fatalf("%+v", o)
	}
	if o.Lines[0].UnitPriceCents != nil {
		t.Fatal("price should be absent")
	}
}

func TestParseRejects(t *testing.T) {
	cases := []string{
		`{}`,
		`{"id":"x","fulfillment":{"type":"teleport"},"customer":{"name":"A","email":"a@b.nl"},"lines":[{"sku":"s","quantity":1}],"payment":{"mode":"paid","reference":"tr_1"}}`,
		`{"id":"x","fulfillment":{"type":"bezorg"},"customer":{"name":"A","email":"a@b.nl"},"lines":[{"sku":"s","quantity":1}],"payment":{"mode":"paid","reference":"tr_1"}}`,
		`{"id":"x","fulfillment":{"type":"afhaal"},"customer":{"name":"A"},"lines":[{"sku":"s","quantity":1}],"payment":{"mode":"paid","reference":"tr_1"}}`,
		`{"id":"x","fulfillment":{"type":"afhaal"},"customer":{"name":"A","email":"a@b.nl"},"lines":[],"payment":{"mode":"paid","reference":"tr_1"}}`,
		`{"id":"x","fulfillment":{"type":"afhaal"},"customer":{"name":"A","email":"a@b.nl"},"lines":[{"sku":"s","quantity":1}],"payment":{"mode":"paid"}}`,
		`{"id":"x","fulfillment":{"type":"afhaal"},"customer":{"name":"A","email":"a@b.nl"},"lines":[{"sku":"s","quantity":1}],"payment":{"mode":"op_rekening"}}`,
		`{"id":"has space","fulfillment":{"type":"afhaal"},"customer":{"name":"A","email":"a@b.nl"},"lines":[{"sku":"s","quantity":1}],"payment":{"mode":"unpaid"}}`,
		`not json`,
	}
	for _, c := range cases {
		_, err := Parse([]byte(c))
		if err == nil {
			t.Fatalf("accepted %s", c)
		}
		if c == `not json` {
			var ve *ValidationError
			if errors.As(err, &ve) {
				t.Fatalf("json garbage should not be a validation error: %v", err)
			}
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want validation error for %s, got %v", c, err)
		}
	}
}

func TestResolveSKU(t *testing.T) {
	m := map[string]string{"site-broodje": "BROODJE-GEZOND", "blank": "  "}
	if got := ResolveSKU("site-broodje", m); got != "BROODJE-GEZOND" {
		t.Fatal(got)
	}
	if got := ResolveSKU("already", m); got != "already" {
		t.Fatal(got)
	}
	if got := ResolveSKU("blank", m); got != "blank" {
		t.Fatal(got)
	}
	if got := ResolveSKU("x", nil); got != "x" {
		t.Fatal(got)
	}
}

func TestSignRoundTrip(t *testing.T) {
	body := []byte(sample)
	sig := Sign("secret", body)
	if err := VerifySignature("secret", sig, body); err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature("other", sig, body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if err := VerifySignature("secret", sig, append(body, ' ')); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered: %v", err)
	}
	if err := VerifySignature("", sig, body); !errors.Is(err, channel.ErrNotConfigured) {
		t.Fatalf("empty secret: %v", err)
	}
	if err := VerifySignature("secret", "deadbeef", body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("no prefix: %v", err)
	}
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatal(sig)
	}
}
