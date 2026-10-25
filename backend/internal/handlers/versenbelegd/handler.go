// Package versenbelegd accepts a signed order from the Vers & Belegd
// ordering site and writes it as a BeepBite order.
//
// The HTTP handler checks the HMAC and parses the body through
// internal/channel/versenbelegd. The store prices the lines off the location's
// own menu, snapshots that location's tax, and inserts the order as
// confirmed so the existing KDS fan-out trigger can see it. Mollie is not
// called. A paid order is recorded as already settled somewhere else.
package versenbelegd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/beepbite/backend/internal/channel"
	intake "github.com/beepbite/backend/internal/channel/versenbelegd"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config turns the rail on. Both the secret and the location slug are
// required; either one missing and the route answers 503. The sku map path
// is optional — empty means the site's sku is already items.sku.
type Config struct {
	WebhookSecret string
	LocationSlug  string
	SKUMapPath    string
}

func (c Config) enabled() (bool, string) {
	if strings.TrimSpace(c.WebhookSecret) == "" {
		return false, "VERS_EN_BELEGD_WEBHOOK_SECRET is empty"
	}
	if strings.TrimSpace(c.LocationSlug) == "" {
		return false, "VERS_EN_BELEGD_LOCATION_SLUG is empty"
	}
	return true, ""
}

// Result is what the ordering site gets back. TotalCents is BeepBite's
// figure, computed from the menu and the location's tax, not the total the
// site sent (the site does not send a total; line prices are advisory).
type Result struct {
	OrderID       string   `json:"order_id"`
	OrderNumber   string   `json:"order_number"`
	Status        string   `json:"status"`
	Replayed      bool     `json:"replayed"`
	TotalCents    int64    `json:"total_cents"`
	Currency      string   `json:"currency"`
	PaymentMode   string   `json:"payment_mode"`
	PriceWarnings []string `json:"price_warnings,omitempty"`
	// Kitchen is "queued": the row is confirmed, which enqueues KDS fan-out.
	// The ticket itself appears on the next pass of the fan-out worker
	// (about five seconds) and only for items that route to a station.
	Kitchen string `json:"kitchen"`
}

// Accepter writes one normalised order. The HTTP tests substitute a fake;
// Store is the Postgres implementation.
type Accepter interface {
	Accept(ctx context.Context, locationSlug string, order channel.InboundOrder, skuMap map[string]string) (Result, error)
}

// Handler is the unauthenticated POST /webhooks/vers-en-belegd/orders route.
type Handler struct {
	cfg     Config
	orders  Accepter
	readMap func(path string) (map[string]string, error)
}

// NewHandler builds the route against pool. pool may be nil only when the
// caller replaces orders before any request is served.
func NewHandler(pool *pgxpool.Pool, cfg Config) *Handler {
	return &Handler{
		cfg:     cfg,
		orders:  &Store{pool: pool},
		readMap: intake.LoadSKUMap,
	}
}

// Mount registers the webhook on r. It is unauthenticated: the HMAC is the
// credential. Do not put it behind the staff JWT middleware.
func (h *Handler) Mount(r interface {
	Post(pattern string, h http.HandlerFunc)
}) {
	r.Post("/webhooks/vers-en-belegd/orders", h.create)
}

const maxBody = 1 << 20

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if ok, why := h.cfg.enabled(); !ok {
		log.Printf("vers-en-belegd: rejecting webhook: %s", why)
		writeErr(w, http.StatusServiceUnavailable, "vers-en-belegd intake is not configured")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}

	if err := intake.VerifySignature(h.cfg.WebhookSecret, r.Header.Get(intake.SignatureHeader), body); err != nil {
		if errors.Is(err, channel.ErrNotConfigured) {
			writeErr(w, http.StatusServiceUnavailable, "vers-en-belegd intake is not configured")
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	order, err := intake.Parse(body)
	if err != nil {
		var ve *intake.ValidationError
		if errors.As(err, &ve) {
			writeErr(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	skuMap, err := h.readMap(h.cfg.SKUMapPath)
	if err != nil {
		log.Printf("vers-en-belegd: sku map: %v", err)
		writeErr(w, http.StatusServiceUnavailable, "sku map is not readable")
		return
	}

	var result Result
	var accErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, accErr = h.orders.Accept(r.Context(), strings.TrimSpace(h.cfg.LocationSlug), order, skuMap)
		if accErr == nil || !errors.Is(accErr, ErrConflict) {
			break
		}
	}
	if accErr != nil {
		switch {
		case errors.Is(accErr, ErrLocationNotFound), errors.Is(accErr, ErrLocationInactive):
			writeErr(w, http.StatusNotFound, "location not found")
		case errors.Is(accErr, ErrNoCurrency):
			writeErr(w, http.StatusUnprocessableEntity, "location has no currency configured")
		case errors.As(accErr, new(*UnknownSKUError)):
			var u *UnknownSKUError
			errors.As(accErr, &u)
			writeErr(w, http.StatusUnprocessableEntity, u.Error())
		case errors.As(accErr, new(*SoldOutError)):
			var s *SoldOutError
			errors.As(accErr, &s)
			writeErr(w, http.StatusUnprocessableEntity, s.Error())
		case errors.Is(accErr, ErrHouseAccountNotFound):
			writeErr(w, http.StatusUnprocessableEntity, "house account not found")
		case errors.Is(accErr, ErrHouseAccountAmbiguous):
			writeErr(w, http.StatusUnprocessableEntity, "house account name matches more than one account")
		case errors.Is(accErr, ErrAccountClosed):
			writeErr(w, http.StatusUnprocessableEntity, "house account is closed")
		case errors.Is(accErr, ErrCreditLimit):
			writeErr(w, http.StatusUnprocessableEntity, "house account credit limit would be exceeded")
		case errors.Is(accErr, ErrCurrencyMismatch):
			writeErr(w, http.StatusUnprocessableEntity, "house account currency does not match the location")
		default:
			log.Printf("vers-en-belegd: accept %s: %v", order.ExternalID, accErr)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
