package versenbelegd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/beepbite/backend/internal/bizday"
	"github.com/beepbite/backend/internal/channel"
	intake "github.com/beepbite/backend/internal/channel/versenbelegd"
	"github.com/beepbite/backend/internal/db"
	"github.com/beepbite/backend/internal/linetax"
	"github.com/beepbite/backend/internal/money"
	"github.com/beepbite/backend/internal/tax"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrLocationNotFound      = errors.New("versenbelegd: location not found")
	ErrLocationInactive      = errors.New("versenbelegd: location inactive")
	ErrNoCurrency            = errors.New("versenbelegd: location has no currency")
	ErrHouseAccountNotFound  = errors.New("versenbelegd: house account not found")
	ErrHouseAccountAmbiguous = errors.New("versenbelegd: house account ambiguous")
	ErrAccountClosed         = errors.New("versenbelegd: house account closed")
	ErrCreditLimit           = errors.New("versenbelegd: credit limit")
	ErrCurrencyMismatch      = errors.New("versenbelegd: currency mismatch")
	// ErrConflict means a unique index collided and the transaction was rolled
	// back. The handler retries the whole accept.
	ErrConflict = errors.New("versenbelegd: unique conflict")
)

// UnknownSKUError lists site skus that are not an active menu item here.
type UnknownSKUError struct{ SKUs []string }

func (e *UnknownSKUError) Error() string {
	return "unknown sku: " + strings.Join(e.SKUs, ", ")
}

// SoldOutError lists skus that exist but are 86'd or inactive.
type SoldOutError struct{ SKUs []string }

func (e *SoldOutError) Error() string {
	return "sku not for sale: " + strings.Join(e.SKUs, ", ")
}

// Store writes one webhook order into Postgres.
//
// The transaction starts as service role so the location can be resolved
// without a staff session. It then narrows to that location's organization.
// Inserting the order as confirmed fires queue_kds_fanout, and that trigger
// clears the service-role flag; the organization id is what the item and
// payment inserts after it are allowed by.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

type resolvedLine struct {
	itemID string
	qty    int
	unit   int64
	notes  string
}

// Accept inserts the order, or returns the row already stored for this
// external id.
func (s *Store) Accept(ctx context.Context, locationSlug string, order channel.InboundOrder, skuMap map[string]string) (Result, error) {
	var result Result
	err := db.Scoped(ctx, s.pool, db.ServiceRoleScope(), func(tx pgx.Tx) error {
		loc, err := loadLocation(ctx, tx, locationSlug)
		if err != nil {
			return err
		}
		if err := db.ApplyScope(ctx, tx, db.Scope{OrgID: loc.orgID, IsServiceRole: true}); err != nil {
			return err
		}

		key := intake.IdempotencyKey(order.ExternalID)
		if existing, found, err := findExisting(ctx, tx, key); err != nil {
			return err
		} else if found {
			if existing.locationID != loc.id {
				return fmt.Errorf("idempotency key %s already belongs to another location", key)
			}
			result = existing.result
			result.Replayed = true
			result.PaymentMode = order.Payment.Mode
			result.Kitchen = "queued"
			result.TaxLines, result.TaxCents, err = loadTaxLines(ctx, tx, result.OrderID)
			return err
		}

		lines, warnings, err := resolveLines(ctx, tx, loc.id, loc.decimals, order.Lines, skuMap)
		if err != nil {
			return err
		}

		var subtotal int64
		priced := make([]linetax.PricedLine, len(lines))
		for i, ln := range lines {
			amount := ln.unit * int64(ln.qty)
			subtotal += amount
			priced[i] = linetax.PricedLine{ItemID: ln.itemID, Amount: amount}
		}

		// SKUs with no item rate use the location default, which is what this
		// path charged on the whole order before per-item rates existed.
		split, err := linetax.Price(ctx, tx, loc.id, priced)
		if err != nil {
			return err
		}
		fee, err := deliveryFeeCents(loc, order.Fulfillment.Type, subtotal)
		if err != nil {
			return err
		}
		total := split.Gross + fee

		var accountID string
		if order.Payment.Mode == "on_account" {
			accountID, err = lockHouseAccount(ctx, tx, loc.orgID, loc.currency, order.Payment.HouseAccountName, total)
			if err != nil {
				return err
			}
		}

		customerID, err := insertCustomer(ctx, tx, loc.orgID, order.Customer)
		if err != nil {
			return err
		}

		zone := bizday.Zone(loc.timezone)
		businessDate := bizday.Date(s.clock(), zone)
		number, err := nextOrderNumber(ctx, tx, loc.id, businessDate)
		if err != nil {
			return err
		}

		var pickup, eta any
		if !order.Fulfillment.RequestedFor.IsZero() {
			if order.Fulfillment.Type == "delivery" {
				eta = order.Fulfillment.RequestedFor
			} else {
				pickup = order.Fulfillment.RequestedFor
			}
		}
		var deliveryAddr, deliveryInstr any
		if order.Fulfillment.Type == "delivery" {
			deliveryAddr = formatAddress(order.Fulfillment.Address)
			if order.Fulfillment.Address.Notes != "" {
				deliveryInstr = order.Fulfillment.Address.Notes
			}
		}

		var orderID string
		err = tx.QueryRow(ctx, `
			INSERT INTO orders (
				organization_id, location_id, customer_id, order_number,
				order_type, fulfillment_type, status,
				subtotal_cents, delivery_fee_cents, tax_cents, total_cents,
				tax_rate, tax_inclusive, tax_mixed, currency_code,
				delivery_address, delivery_instructions,
				business_date, idempotency_key,
				notes, kitchen_notes, customer_note,
				pickup_at, estimated_delivery_time
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, 'confirmed',
				$7, $8, $9, $10,
				$11, $12, $13, $14,
				$15, $16,
				$17::date, $18,
				$19, $20, $21,
				$22, $23
			) RETURNING id::text
		`,
			loc.orgID, loc.id, customerID, number,
			orderType(order.Fulfillment.Type), order.Fulfillment.Type,
			subtotal, fee, split.Tax, total,
			split.HeaderRate, split.HeaderInclusive, split.Mixed, loc.currency,
			deliveryAddr, deliveryInstr,
			businessDate, key,
			internalNotes(order, warnings), kitchenNotes(order, zone), nullIfEmpty(customerNote(order)),
			pickup, eta,
		).Scan(&orderID)
		if isConflict(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}

		for i, ln := range lines {
			rated := split.Lines[i]
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_items (
					order_id, item_id, quantity, unit_price_cents, total_price_cents, special_instructions,
					tax_rate, tax_inclusive, tax_label, net_cents, tax_cents
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			`, orderID, ln.itemID, ln.qty, ln.unit, ln.unit*int64(ln.qty), nullIfEmpty(ln.notes),
				rated.Percent, rated.Inclusive, rated.Label, rated.Net, rated.Tax); err != nil {
				return fmt.Errorf("insert order item: %w", err)
			}
		}
		if err := linetax.ReplaceLines(ctx, tx, orderID, split.Buckets); err != nil {
			return err
		}

		if err := recordPayment(ctx, tx, order, orderID, customerID, accountID, total); err != nil {
			return err
		}

		result = Result{
			OrderID:       orderID,
			OrderNumber:   number,
			Status:        "confirmed",
			TotalCents:    total,
			TaxCents:      split.Tax,
			TaxLines:      split.Buckets,
			Currency:      loc.currency,
			PaymentMode:   order.Payment.Mode,
			PriceWarnings: warnings,
			Kitchen:       "queued",
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

type location struct {
	id            string
	orgID         string
	timezone      string
	currency      string
	decimals      int
	taxRate       float64
	taxInclusive  bool
	taxLabel      string
	deliveryFee   string
	freeThreshold string
	active        bool
}

func loadLocation(ctx context.Context, tx pgx.Tx, slug string) (location, error) {
	var loc location
	err := tx.QueryRow(ctx, `
		SELECT l.id::text, l.organization_id::text, COALESCE(l.timezone, 'UTC'),
		       COALESCE(l.currency_code, ''),
		       COALESCE(c.decimal_digits, 2),
		       CAST(l.tax_rate AS float8), l.tax_inclusive, COALESCE(l.tax_label, ''),
		       l.delivery_fee::text, l.free_delivery_threshold::text,
		       l.is_active
		FROM locations l
		LEFT JOIN currencies c ON c.code = l.currency_code
		WHERE l.slug = $1
	`, slug).Scan(
		&loc.id, &loc.orgID, &loc.timezone,
		&loc.currency, &loc.decimals,
		&loc.taxRate, &loc.taxInclusive, &loc.taxLabel,
		&loc.deliveryFee, &loc.freeThreshold,
		&loc.active,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return location{}, ErrLocationNotFound
	}
	if err != nil {
		return location{}, err
	}
	if !loc.active {
		return location{}, ErrLocationInactive
	}
	if loc.currency == "" {
		return location{}, ErrNoCurrency
	}
	if loc.decimals < 0 || loc.decimals > money.MaxDecimals {
		loc.decimals = 2
	}
	return loc, nil
}

type existingOrder struct {
	locationID string
	result     Result
}

func findExisting(ctx context.Context, tx pgx.Tx, key string) (existingOrder, bool, error) {
	var ex existingOrder
	err := tx.QueryRow(ctx, `
		SELECT id::text, order_number, status::text, total_cents,
		       COALESCE(currency_code, ''), location_id::text
		FROM orders
		WHERE idempotency_key = $1
	`, key).Scan(&ex.result.OrderID, &ex.result.OrderNumber, &ex.result.Status,
		&ex.result.TotalCents, &ex.result.Currency, &ex.locationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return existingOrder{}, false, nil
	}
	if err != nil {
		return existingOrder{}, false, err
	}
	return ex, true, nil
}

func resolveLines(ctx context.Context, tx pgx.Tx, locationID string, decimals int, lines []channel.OrderLine, skuMap map[string]string) ([]resolvedLine, []string, error) {
	var out []resolvedLine
	var warnings []string
	var unknown, sold []string
	for _, ln := range lines {
		sku := intake.ResolveSKU(ln.SKU, skuMap)
		var (
			itemID, priceText   string
			active, eightySixed bool
		)
		err := tx.QueryRow(ctx, `
			SELECT id::text, price::text, is_active, is_86ed
			FROM items
			WHERE location_id = $1 AND sku = $2
		`, locationID, sku).Scan(&itemID, &priceText, &active, &eightySixed)
		if errors.Is(err, pgx.ErrNoRows) {
			unknown = append(unknown, ln.SKU)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !active || eightySixed {
			sold = append(sold, ln.SKU)
			continue
		}
		// items.price is major units. Parse with the location currency's
		// exponent so a 0-decimal currency is not treated as cents.
		unit, err := money.Parse(priceText, decimals)
		if err != nil {
			return nil, nil, fmt.Errorf("item %s price %q: %w", sku, priceText, err)
		}
		out = append(out, resolvedLine{itemID: itemID, qty: ln.Quantity, unit: unit, notes: ln.Notes})
		if ln.UnitPriceCents != nil && *ln.UnitPriceCents != unit {
			warnings = append(warnings, fmt.Sprintf("sku %s: site %d, menu %d (minor units); menu price used", ln.SKU, *ln.UnitPriceCents, unit))
		}
	}
	if len(unknown) > 0 {
		return nil, nil, &UnknownSKUError{SKUs: unknown}
	}
	if len(sold) > 0 {
		return nil, nil, &SoldOutError{SKUs: sold}
	}
	return out, warnings, nil
}

func loadTaxLines(ctx context.Context, tx pgx.Tx, orderID string) ([]tax.BreakdownLine, int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT tax_label, CAST(tax_rate AS float8), tax_inclusive, net_cents, tax_cents, gross_cents
		FROM order_tax_lines
		WHERE order_id = $1
		ORDER BY tax_rate, tax_label
	`, orderID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var lines []tax.BreakdownLine
	var taxSum int64
	for rows.Next() {
		var ln tax.BreakdownLine
		if err := rows.Scan(&ln.Label, &ln.Rate, &ln.Inclusive, &ln.NetCents, &ln.TaxCents, &ln.GrossCents); err != nil {
			return nil, 0, err
		}
		taxSum += ln.TaxCents
		lines = append(lines, ln)
	}
	return lines, taxSum, rows.Err()
}

func deliveryFeeCents(loc location, fulfillment string, subtotal int64) (int64, error) {
	if fulfillment != "delivery" {
		return 0, nil
	}
	fee, err := money.Parse(loc.deliveryFee, loc.decimals)
	if err != nil {
		return 0, fmt.Errorf("delivery fee: %w", err)
	}
	threshold, err := money.Parse(loc.freeThreshold, loc.decimals)
	if err != nil {
		return 0, fmt.Errorf("free-delivery threshold: %w", err)
	}
	// A threshold of 0 is the schema default ("no rule"), not "always free".
	if threshold > 0 && subtotal >= threshold {
		return 0, nil
	}
	if fee < 0 {
		return 0, nil
	}
	return fee, nil
}

func lockHouseAccount(ctx context.Context, tx pgx.Tx, orgID, currency, name string, total int64) (string, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, currency, is_active, credit_limit_cents
		FROM house_accounts
		WHERE organization_id = $1 AND lower(account_name) = lower($2)
		FOR UPDATE
	`, orgID, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type acc struct {
		id       string
		currency string
		active   bool
		limit    *int64
	}
	var found []acc
	for rows.Next() {
		var a acc
		if err := rows.Scan(&a.id, &a.currency, &a.active, &a.limit); err != nil {
			return "", err
		}
		found = append(found, a)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", ErrHouseAccountNotFound
	}
	if len(found) > 1 {
		return "", ErrHouseAccountAmbiguous
	}
	a := found[0]
	if !a.active {
		return "", ErrAccountClosed
	}
	if !strings.EqualFold(a.currency, currency) {
		return "", ErrCurrencyMismatch
	}
	var outstanding int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0)
		FROM house_account_charges
		WHERE house_account_id = $1 AND house_account_invoice_id IS NULL
	`, a.id).Scan(&outstanding); err != nil {
		return "", err
	}
	if a.limit != nil && outstanding+total > *a.limit {
		return "", ErrCreditLimit
	}
	return a.id, nil
}

func insertCustomer(ctx context.Context, tx pgx.Tx, orgID string, c channel.Customer) (string, error) {
	if c.Email != "" {
		var id string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM customers
			WHERE organization_id = $1 AND lower(email) = $2
			ORDER BY created_at
			LIMIT 1
		`, orgID, c.Email).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	first, last := splitName(c.Name)
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO customers (organization_id, first_name, last_name, email, notes)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
		RETURNING id::text
	`, orgID, first, last, c.Email, customerRecordNote(c)).Scan(&id)
	return id, err
}

func nextOrderNumber(ctx context.Context, tx pgx.Tx, locationID, businessDate string) (string, error) {
	var maxSeq int
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(
			CASE
				WHEN order_number ~ '^VB([0-9]+)$' THEN (regexp_replace(order_number, '^VB', ''))::int
				ELSE 0
			END
		), 0)
		FROM orders
		WHERE location_id = $1 AND business_date = $2::date
	`, locationID, businessDate).Scan(&maxSeq)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("VB%04d", maxSeq+1), nil
}

func recordPayment(ctx context.Context, tx pgx.Tx, order channel.InboundOrder, orderID, customerID, accountID string, total int64) error {
	switch order.Payment.Mode {
	case "paid":
		provider := order.Payment.Provider
		if provider == "" {
			provider = "external"
		}
		note := fmt.Sprintf("Paid on Vers & Belegd via %s (%s). BeepBite did not take this payment.", provider, order.Payment.Reference)
		_, err := tx.Exec(ctx, `
			INSERT INTO order_payments (
				order_id, payment_method_code, amount_paid_cents,
				payment_reference, external_transaction_id, payment_status, notes
			) VALUES ($1, 'external_prepaid', $2, $3, $3, 'completed', $4)
		`, orderID, total, order.Payment.Reference, note)
		if err != nil {
			return fmt.Errorf("insert prepaid tender: %w", err)
		}
	case "on_account":
		if _, err := tx.Exec(ctx, `
			INSERT INTO house_account_charges (house_account_id, order_id, customer_id, amount_cents)
			VALUES ($1, $2, $3, $4)
		`, accountID, orderID, customerID, total); err != nil {
			return fmt.Errorf("insert house charge: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE house_accounts
			SET current_balance_cents = current_balance_cents + $2, updated_at = now()
			WHERE id = $1
		`, accountID, total); err != nil {
			return fmt.Errorf("update house balance: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO order_payments (
				order_id, payment_method_code, amount_paid_cents,
				payment_reference, payment_status, notes
			) VALUES ($1, 'house_account', $2, $3, 'completed', $4)
		`, orderID, total, order.Payment.HouseAccountName,
			"Charged to house account from a Vers & Belegd order. Invoice it from the house account; do not take the money again at the counter.")
		if err != nil {
			return fmt.Errorf("insert house tender: %w", err)
		}
	}
	return nil
}

func isConflict(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
