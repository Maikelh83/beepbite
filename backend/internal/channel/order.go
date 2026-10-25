package channel

import "time"

// OrderSource is the structured-order half of this package.
//
// Channel and Inbound describe a conversation: a customer message comes in,
// a reply goes out, and richness degrades to text when the rail cannot render
// it. A hosted checkout does not have that conversation. It posts one order
// that is already placed — line items, how it will be fulfilled, who it is
// for, and an id the sender will retry with.
//
// Putting that payload through InboundMessage.Text would force the kitchen
// path to re-parse a sentence. OrderSource keeps the same rule as Channel:
// the caller depends on this package, not on a particular site's JSON, and
// the adapter is the only place that knows the site's field names.
type OrderSource interface {
	// Name is persisted and logged. Stable. "vers-en-belegd", not a display name.
	Name() string

	// ParseOrder turns one raw webhook body into a normalised order.
	// A body that is not a coherent order returns an error the caller can
	// show back to the sender; it must not return a partial order.
	ParseOrder(body []byte) (InboundOrder, error)
}

// InboundOrder is one finished order, independent of which site sent it.
//
// Money on the lines is what the site claims the customer was shown. The
// caller decides whether that price is allowed to win over the menu price.
// This struct does not apply tax, look up a menu, or talk to a database.
type InboundOrder struct {
	// ExternalID is the sender's own order id. The caller namespaces it into
	// orders.idempotency_key. Empty is not a valid order.
	ExternalID string

	// PlacedAt is the sender's clock when the customer checked out, when the
	// payload carries one. The zero value means the sender did not say.
	PlacedAt time.Time

	Fulfillment Fulfillment
	Customer    Customer
	Lines       []OrderLine
	Payment     Payment

	// Notes is the customer's free-text remark, already trimmed.
	Notes string
}

// Fulfillment is how this order reaches the customer.
//
// Type is one of "delivery", "collection", "dine_in" — the same words as
// BeepBite's fulfillment_type enum. Adapters map site-specific words
// ("bezorg", "afhaal") onto these before returning.
type Fulfillment struct {
	Type string

	// RequestedFor is when the customer wants the order, in absolute time.
	// The zero value means "as soon as the kitchen can".
	RequestedFor time.Time

	Address Address
}

// Address is a delivery or company address. Every field may be empty;
// the caller decides which combinations are acceptable for a given Type.
type Address struct {
	Company    string
	Line1      string
	Line2      string
	PostalCode string
	City       string
	Notes      string
}

// Customer is who placed the order. Name is the person. Company is the
// account the lunch is for, which may differ from Address.Company when a
// head office orders for a site.
type Customer struct {
	Name       string
	Email      string
	Phone      string
	Company    string
	CostCenter string
}

// OrderLine is one menu line. SKU is the site's product code; the caller
// maps it onto items.sku. UnitPriceCents is nil when the site did not send
// a price. Quantity is a whole number of the sellable unit (one broodje,
// one platter), not a weight.
type OrderLine struct {
	SKU            string
	Name           string
	Quantity       int
	UnitPriceCents *int64
	Notes          string
}

// Payment describes how the sender says the order was, or will be, settled.
//
// Mode is one of:
//
//	"paid"       the sender already took the money (for example Mollie)
//	"on_account" charge a house account ("op rekening")
//	"unpaid"     the counter or the door still has to collect
//
// SettleAt distinguishes the two unpaid cases the kitchen cares about:
// "pickup", "delivery", or "" when Mode is not "unpaid".
//
// BeepBite does not call the payment provider. Reference is stored so a
// person can reconcile; it is not a credential.
type Payment struct {
	Mode             string
	SettleAt         string
	Provider         string
	Reference        string
	HouseAccountName string
}
