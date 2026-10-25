// Package linetax resolves the rate for each sold item and writes the
// per-rate receipt rows.
//
// An item with tax_rate_id set uses that named rate, which must be active and
// belong to the same location. An item with no rate uses the location
// default: the oldest active tax_rates row, otherwise the location's own
// tax_rate / tax_inclusive / tax_label. That is the same default a sale used
// when every line shared one rate, so an item that was never assigned a rate
// does not change price.
//
// The lookup runs on the caller's transaction. tax_rates has FORCE ROW LEVEL
// SECURITY. A query on a fresh pool connection, with no app.current_org_id,
// comes back empty and is indistinguishable from "this location has no named
// rate", which would silently charge the wrong tax. The caller has already
// set the org scope (or service role) on tx.
package linetax

import (
	"context"
	"errors"
	"fmt"

	"github.com/beepbite/backend/internal/tax"
	"github.com/jackc/pgx/v5"
)

// ErrUnavailable means the item points at a rate that is inactive, missing,
// or on another location. The sale does not fall back to a different rate:
// that would charge a tax the operator did not choose.
var ErrUnavailable = errors.New("item tax rate is inactive or not on this location")

// PricedLine is one line's extended menu price. Override, when set, is a
// rate already snapshotted on a previous version of this line. Editing an
// open ticket keeps that rate so a settings change during service does not
// restate a line the customer already saw. A new item leaves Override nil
// and is resolved from the menu.
type PricedLine struct {
	ItemID   string
	Amount   int64
	Override *tax.Line
}

// Price resolves a rate for each line and splits the tax.
func Price(ctx context.Context, tx pgx.Tx, locationID string, lines []PricedLine) (tax.Split, error) {
	need := make([]string, 0, len(lines))
	seen := map[string]struct{}{}
	for _, ln := range lines {
		if ln.Override != nil {
			continue
		}
		if _, ok := seen[ln.ItemID]; ok {
			continue
		}
		seen[ln.ItemID] = struct{}{}
		need = append(need, ln.ItemID)
	}

	rates, err := resolve(ctx, tx, locationID, need)
	if err != nil {
		return tax.Split{}, err
	}

	out := make([]tax.Line, len(lines))
	for i, ln := range lines {
		if ln.Override != nil {
			out[i] = *ln.Override
			out[i].Amount = ln.Amount
			continue
		}
		rate, ok := rates[ln.ItemID]
		if !ok {
			return tax.Split{}, fmt.Errorf("%w: %s", ErrUnavailable, ln.ItemID)
		}
		out[i] = tax.Line{
			Amount:    ln.Amount,
			Percent:   rate.percent,
			Inclusive: rate.inclusive,
			Label:     rate.label,
		}
	}
	return tax.SplitLines(out), nil
}

type resolved struct {
	percent   float64
	inclusive bool
	label     string
}

func resolve(ctx context.Context, tx pgx.Tx, locationID string, itemIDs []string) (map[string]resolved, error) {
	out := make(map[string]resolved, len(itemIDs))
	if len(itemIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT i.id::text,
		       (i.tax_rate_id IS NOT NULL AND tr.id IS NULL) AS broken,
		       COALESCE(CAST(tr.rate AS float8), CAST(def.rate AS float8), CAST(l.tax_rate AS float8)),
		       COALESCE(tr.is_inclusive, def.is_inclusive, l.tax_inclusive),
		       COALESCE(NULLIF(tr.name, ''), NULLIF(def.name, ''), NULLIF(l.tax_label, ''), '')
		FROM items i
		JOIN locations l ON l.id = i.location_id
		LEFT JOIN tax_rates tr
		  ON tr.id = i.tax_rate_id
		 AND tr.location_id = i.location_id
		 AND tr.is_active
		LEFT JOIN LATERAL (
			SELECT rate, is_inclusive, name
			FROM tax_rates
			WHERE location_id = l.id
			  AND is_active = true
			ORDER BY created_at, id
			LIMIT 1
		) def ON true
		WHERE i.location_id = $1
		  AND i.id::text = ANY($2::text[])
	`, locationID, itemIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id        string
			broken    bool
			percent   float64
			inclusive bool
			label     string
		)
		if err := rows.Scan(&id, &broken, &percent, &inclusive, &label); err != nil {
			return nil, err
		}
		if broken {
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, id)
		}
		out[id] = resolved{percent: percent, inclusive: inclusive, label: label}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(itemIDs) {
		return nil, fmt.Errorf("%w: item is not on this location", ErrUnavailable)
	}
	return out, nil
}

// ReplaceLines rewrites the per-rate receipt rows for an order.
func ReplaceLines(ctx context.Context, tx pgx.Tx, orderID string, lines []tax.BreakdownLine) error {
	if _, err := tx.Exec(ctx, `DELETE FROM order_tax_lines WHERE order_id = $1`, orderID); err != nil {
		return fmt.Errorf("clear order tax lines: %w", err)
	}
	for _, ln := range lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_tax_lines (
				order_id, tax_label, tax_rate, tax_inclusive,
				net_cents, tax_cents, gross_cents
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, orderID, ln.Label, ln.Rate, ln.Inclusive, ln.NetCents, ln.TaxCents, ln.GrossCents); err != nil {
			return fmt.Errorf("insert order tax line: %w", err)
		}
	}
	return nil
}
