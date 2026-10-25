// Package versenbelegd adapts the Vers & Belegd ordering site onto
// channel.OrderSource.
//
// The site (Maikelh83/vers-en-belegd-bestellen) is a separate Cloudflare
// Pages app. This package does not call it and does not know its database.
// It parses the JSON that app is expected to POST, verifies the HMAC, and
// stops there. Turning the result into a BeepBite order is the handler's job.
package versenbelegd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/beepbite/backend/internal/channel"
)

// Source is the Vers & Belegd order rail. It has no credentials of its own:
// the webhook secret stays in the handler, because parsing a fixture in a
// test must not require one.
type Source struct{}

// Name is the stable rail id stored on the order (in notes and the
// idempotency key), not the customer-facing brand.
func (Source) Name() string { return "vers-en-belegd" }

// ParseOrder implements channel.OrderSource.
func (s Source) ParseOrder(body []byte) (channel.InboundOrder, error) {
	return Parse(body)
}

// Compile-time check that the rail stays on the seam.
var _ channel.OrderSource = Source{}

// IdempotencyKey is the orders.idempotency_key for a Vers & Belegd order id.
//
// The column is unique across the whole database, not per location, so the
// prefix keeps a site id from colliding with a POS client_id or another rail
// that happens to use the same string.
func IdempotencyKey(externalID string) string {
	return "vers-en-belegd:" + externalID
}

// ValidationError is a payload the site can fix by changing the JSON.
// Anything else from Parse is a broken body (not JSON, truncated).
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// wire is the documented POST body. Unknown JSON fields are ignored so the
// site can add a field before this adapter knows about it; the fields below
// are the ones that change a kitchen ticket.
type wire struct {
	ID          string       `json:"id"`
	PlacedAt    string       `json:"placed_at"`
	Fulfillment wireFulfill  `json:"fulfillment"`
	Customer    wireCustomer `json:"customer"`
	Lines       []wireLine   `json:"lines"`
	Payment     wirePayment  `json:"payment"`
	Notes       string       `json:"notes"`
}

type wireFulfill struct {
	Type         string      `json:"type"`
	RequestedFor string      `json:"requested_for"`
	Address      wireAddress `json:"address"`
}

type wireAddress struct {
	Company    string `json:"company"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	PostalCode string `json:"postal_code"`
	City       string `json:"city"`
	Notes      string `json:"notes"`
}

type wireCustomer struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	Company    string `json:"company"`
	CostCenter string `json:"cost_center"`
}

type wireLine struct {
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents *int64 `json:"unit_price_cents"`
	Notes          string `json:"notes"`
}

type wirePayment struct {
	Mode             string `json:"mode"`
	Provider         string `json:"provider"`
	Reference        string `json:"reference"`
	HouseAccountName string `json:"house_account_name"`
}

const (
	maxLines    = 80
	maxQty      = 500
	maxIDRunes  = 120
	maxSKURunes = 80
	maxNote     = 2000
	maxPrice    = int64(10_000_000) // 100,000.00 in a 2-decimal currency
)

// Parse decodes and validates one webhook body.
func Parse(body []byte) (channel.InboundOrder, error) {
	if len(body) == 0 {
		return channel.InboundOrder{}, invalid("body is empty")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	var w wire
	if err := dec.Decode(&w); err != nil {
		return channel.InboundOrder{}, fmt.Errorf("vers-en-belegd: json: %w", err)
	}

	id := strings.TrimSpace(w.ID)
	if id == "" {
		return channel.InboundOrder{}, invalid("id is required")
	}
	if utf8.RuneCountInString(id) > maxIDRunes || !idChars(id) {
		return channel.InboundOrder{}, invalid("id must be 1..%d characters of letters, digits, and . _ : -", maxIDRunes)
	}

	ft, err := fulfillmentType(w.Fulfillment.Type)
	if err != nil {
		return channel.InboundOrder{}, err
	}
	requested, err := optionalTime(w.Fulfillment.RequestedFor, "fulfillment.requested_for")
	if err != nil {
		return channel.InboundOrder{}, err
	}
	placed, err := optionalTime(w.PlacedAt, "placed_at")
	if err != nil {
		return channel.InboundOrder{}, err
	}

	addr := channel.Address{
		Company:    clip(w.Fulfillment.Address.Company, 200),
		Line1:      clip(w.Fulfillment.Address.Line1, 200),
		Line2:      clip(w.Fulfillment.Address.Line2, 200),
		PostalCode: clip(w.Fulfillment.Address.PostalCode, 20),
		City:       clip(w.Fulfillment.Address.City, 120),
		Notes:      clip(w.Fulfillment.Address.Notes, 500),
	}
	if ft == "delivery" && addr.Line1 == "" {
		return channel.InboundOrder{}, invalid("fulfillment.address.line1 is required for delivery")
	}

	cust := channel.Customer{
		Name:       clip(w.Customer.Name, 200),
		Email:      strings.ToLower(strings.TrimSpace(w.Customer.Email)),
		Phone:      compactPhone(w.Customer.Phone),
		Company:    clip(w.Customer.Company, 200),
		CostCenter: clip(w.Customer.CostCenter, 80),
	}
	if cust.Name == "" {
		return channel.InboundOrder{}, invalid("customer.name is required")
	}
	if cust.Email == "" && cust.Phone == "" {
		return channel.InboundOrder{}, invalid("customer.email or customer.phone is required")
	}
	if cust.Email != "" && !strings.Contains(cust.Email, "@") {
		return channel.InboundOrder{}, invalid("customer.email is not an email address")
	}

	if len(w.Lines) == 0 {
		return channel.InboundOrder{}, invalid("lines must not be empty")
	}
	if len(w.Lines) > maxLines {
		return channel.InboundOrder{}, invalid("lines exceed %d", maxLines)
	}
	lines := make([]channel.OrderLine, 0, len(w.Lines))
	for i, ln := range w.Lines {
		sku := strings.TrimSpace(ln.SKU)
		if sku == "" || utf8.RuneCountInString(sku) > maxSKURunes {
			return channel.InboundOrder{}, invalid("lines[%d].sku is required and must be at most %d characters", i, maxSKURunes)
		}
		if ln.Quantity < 1 || ln.Quantity > maxQty {
			return channel.InboundOrder{}, invalid("lines[%d].quantity must be 1..%d", i, maxQty)
		}
		if ln.UnitPriceCents != nil && (*ln.UnitPriceCents < 0 || *ln.UnitPriceCents > maxPrice) {
			return channel.InboundOrder{}, invalid("lines[%d].unit_price_cents is out of range", i)
		}
		var price *int64
		if ln.UnitPriceCents != nil {
			v := *ln.UnitPriceCents
			price = &v
		}
		lines = append(lines, channel.OrderLine{
			SKU:            sku,
			Name:           clip(ln.Name, 200),
			Quantity:       ln.Quantity,
			UnitPriceCents: price,
			Notes:          clip(ln.Notes, 500),
		})
	}

	mode, settle, err := paymentMode(w.Payment.Mode)
	if err != nil {
		return channel.InboundOrder{}, err
	}
	ref := strings.TrimSpace(w.Payment.Reference)
	if mode == "paid" && ref == "" {
		return channel.InboundOrder{}, invalid("payment.reference is required when payment.mode is paid")
	}
	account := strings.TrimSpace(w.Payment.HouseAccountName)
	if mode == "on_account" && account == "" {
		return channel.InboundOrder{}, invalid("payment.house_account_name is required when payment.mode is on_account")
	}
	provider := strings.ToLower(strings.TrimSpace(w.Payment.Provider))
	if utf8.RuneCountInString(provider) > 40 {
		return channel.InboundOrder{}, invalid("payment.provider is too long")
	}
	if utf8.RuneCountInString(ref) > 200 {
		return channel.InboundOrder{}, invalid("payment.reference is too long")
	}

	return channel.InboundOrder{
		ExternalID: id,
		PlacedAt:   placed,
		Fulfillment: channel.Fulfillment{
			Type:         ft,
			RequestedFor: requested,
			Address:      addr,
		},
		Customer: cust,
		Lines:    lines,
		Payment: channel.Payment{
			Mode:             mode,
			SettleAt:         settle,
			Provider:         provider,
			Reference:        ref,
			HouseAccountName: clip(account, 200),
		},
		Notes: clip(w.Notes, maxNote),
	}, nil
}

func fulfillmentType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "delivery", "bezorg", "bezorgen", "bezorging":
		return "delivery", nil
	case "pickup", "collection", "afhaal", "afhalen":
		return "collection", nil
	case "dine_in", "ter_plaatse":
		return "dine_in", nil
	default:
		return "", invalid("fulfillment.type must be delivery, collection, or dine_in (bezorg / afhaal are accepted)")
	}
}

func paymentMode(raw string) (mode, settleAt string, err error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "paid", "betaald":
		return "paid", "", nil
	case "on_account", "op_rekening", "house_account", "rekening":
		return "on_account", "", nil
	case "pay_on_pickup", "pay_on_collection", "afhaal_betalen", "unpaid":
		return "unpaid", "pickup", nil
	case "pay_on_delivery", "bezorg_betalen":
		return "unpaid", "delivery", nil
	default:
		return "", "", invalid("payment.mode must be paid, on_account, pay_on_pickup, or pay_on_delivery")
	}
}

func optionalTime(raw, field string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, invalid("%s must be RFC3339, for example 2026-10-25T11:30:00+02:00", field)
	}
	return t, nil
}

func idChars(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}

func compactPhone(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r == '-' || r == '(' || r == ')' {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > 32 {
		return clip(out, 32)
	}
	return out
}
