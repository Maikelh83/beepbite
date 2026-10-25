package pos

// modify_order.go — Wave 24: "order modification before fire"
//
// PATCH /pos/orders/{order_id}/items
//
// Contract: full-replace — the body is the *desired* set of line items. The
// handler deletes all existing order_items for the order and inserts the new
// set (plus modifiers), recomputes subtotal/tax/total, then re-fanouts to KDS.
//
// Guard: rejected with 409 if any kds_ticket for the order has advanced past
// 'fired' (i.e. status IN ('in_progress', 'ready', 'bumped')).
//
// MountModify registers the single PATCH route at /pos/orders/{order_id}/items
// on the provided chi.Router. The orchestrator calls h.MountModify(r) after
// the existing h.Mount(r) call — it does NOT modify handler.go or store.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/beepbite/backend/internal/auth"
	"github.com/beepbite/backend/internal/db"
	"github.com/beepbite/backend/internal/linetax"
	"github.com/beepbite/backend/internal/locations"
	"github.com/beepbite/backend/internal/money"
	"github.com/beepbite/backend/internal/tax"
)

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

// ErrOrderAlreadyInPrep is returned when any KDS ticket for the order has
// advanced past 'fired' (in_progress / ready / bumped).
var ErrOrderAlreadyInPrep = errors.New("order already in preparation, cannot modify")

// ---------------------------------------------------------------------------
// MountModify registers the modify-order route.
// Call this from the orchestrator (main/server setup) AFTER h.Mount(r):
//
//	h.Mount(r)
//	h.MountModify(r)
//
// ---------------------------------------------------------------------------

// MountModify registers PATCH /pos/orders/{order_id}/items under r.
// It is a separate method in this file so handler.go is not touched.
func (h *Handler) MountModify(r chi.Router) {
	r.Patch("/pos/orders/{order_id}/items", h.modifyOrderItems)
}

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

// modifyOrderReq is the body for PATCH /pos/orders/{order_id}/items.
// It is a full-replace: items is the *desired* set after the edit.
type modifyOrderReq struct {
	Items []OrderLineInput `json:"items"`
}

// ModifiedOrder is returned on a successful modification.
//
// As with CreatedOrder, the *_minor fields are authoritative integer minor
// units and CurrencyDecimals says how many make a major unit. The float fields
// are the legacy shape, now scaled by the currency's real exponent instead of a
// literal 100.
type ModifiedOrder struct {
	OrderID string `json:"order_id"`

	SubtotalMinor int64 `json:"subtotal_minor"`
	TaxMinor      int64 `json:"tax_minor"`
	TotalMinor    int64 `json:"total_minor"`

	Subtotal float64 `json:"subtotal"`
	Tax      float64 `json:"tax"`
	Total    float64 `json:"total"`

	CurrencyCode     string `json:"currency_code"`
	CurrencyDecimals int    `json:"currency_decimals"`

	TaxRate      float64 `json:"tax_rate"`
	TaxInclusive bool    `json:"tax_inclusive"`

	KDSTicketIDs []string `json:"kds_ticket_ids"`
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// PATCH /pos/orders/{order_id}/items
func (h *Handler) modifyOrderItems(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "order_id")
	if orderID == "" {
		writeErr(w, http.StatusBadRequest, "order_id is required")
		return
	}

	var req modifyOrderReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "items must not be empty")
		return
	}
	for i, line := range req.Items {
		if line.ItemID == "" {
			writeErr(w, http.StatusBadRequest, "items["+itoa(i)+"]: item_id is required")
			return
		}
		if line.Quantity <= 0 {
			writeErr(w, http.StatusBadRequest, "items["+itoa(i)+"]: quantity must be > 0")
			return
		}
	}

	// Org-scope check: verify the order belongs to a location the caller owns.
	scope := auth.OrgScopeFrom(r.Context())
	orderLocID, err := h.store.GetOrderLocationID(r.Context(), orderID)
	switch {
	case errors.Is(err, ErrOrderNotFound):
		writeErr(w, http.StatusNotFound, "order not found")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !scope.AllowsLocation(orderLocID) {
		writeErr(w, http.StatusNotFound, "order not found")
		return
	}

	actorID := scope.UserID

	result, err := h.store.ModifyOrderItems(r.Context(), orderID, req.Items, actorID)
	switch {
	case errors.Is(err, ErrOrderNotFound):
		writeErr(w, http.StatusNotFound, "order not found")
	case errors.Is(err, ErrOrderAlreadyInPrep):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrItemNotFound):
		writeErr(w, http.StatusBadRequest, "one or more item_ids are invalid")
	case errors.Is(err, linetax.ErrUnavailable):
		writeErr(w, http.StatusUnprocessableEntity, "an item's tax rate is inactive or not on this location")
	case errors.Is(err, ErrBadModifier):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

// ---------------------------------------------------------------------------
// Store method
// ---------------------------------------------------------------------------

// ModifyOrderItems replaces an order's line items in one transaction:
//  1. Guards: order must exist and no KDS ticket may be past 'fired'.
//  2. Deletes existing order_items (cascades to order_item_modifiers via FK).
//  3. Inserts the new desired set (items + modifiers).
//  4. Recomputes subtotal/tax/total (same cents math as CreateOrder).
//  5. Deletes existing KDS tickets/items still in 'fired' state, then re-fanouts.
//  6. Writes an audit_log row (action 'order.modified').
func (s *Store) ModifyOrderItems(
	ctx context.Context,
	orderID string,
	lines []OrderLineInput,
	actorID string,
) (*ModifiedOrder, error) {
	scope := db.ScopeFromContext(ctx)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Set RLS session vars so tenant policies apply.
	if err := setTxScope(ctx, tx, scope); err != nil {
		return nil, err
	}

	// --- 1. Lock the order and resolve location_id / organization_id ---
	// Currency stays the one snapshotted on the order. Re-resolving it from
	// the location would relabel a sale the customer already saw.
	//
	// Tax is per line. A line that was already on the order keeps the rate
	// snapshotted when it was first sent, so a menu edit during service does
	// not restate it. A line that is new on this edit takes the item's rate
	// now. The order-level rate cannot be reused: a mixed ticket does not
	// have one.
	var locationID, orgID string
	var orderCurrency *string
	err = tx.QueryRow(ctx,
		`SELECT location_id, organization_id, currency_code
		   FROM orders WHERE id = $1 FOR UPDATE`,
		orderID,
	).Scan(&locationID, &orgID, &orderCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}

	// --- 2. Guard: reject if any KDS ticket has advanced past 'fired' ---
	var advancedCount int
	err = tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM kds_tickets
		WHERE order_id = $1
		  AND status IN ('in_progress', 'ready', 'bumped')
	`, orderID).Scan(&advancedCount)
	if err != nil {
		return nil, err
	}
	if advancedCount > 0 {
		return nil, ErrOrderAlreadyInPrep
	}

	// --- 3. Snapshot before-state for audit (item count, old total) ---
	var oldTotalCents int64
	var oldItemCount int
	_ = tx.QueryRow(ctx,
		`SELECT total_cents FROM orders WHERE id = $1`, orderID,
	).Scan(&oldTotalCents)
	_ = tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_items WHERE order_id = $1`, orderID,
	).Scan(&oldItemCount)

	// --- 4. Resolve item prices ---
	type itemRow struct {
		id    string
		price float64
	}
	itemCache := make(map[string]itemRow, len(lines))
	for _, line := range lines {
		if _, cached := itemCache[line.ItemID]; cached {
			continue
		}
		var ir itemRow
		err := tx.QueryRow(ctx,
			`SELECT id, price FROM items WHERE id = $1`, line.ItemID,
		).Scan(&ir.id, &ir.price)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrItemNotFound
		}
		if err != nil {
			return nil, err
		}
		itemCache[line.ItemID] = ir
	}

	// --- 5. Resolve modifier prices and compute per-line unit prices ---
	type resolvedModifier struct {
		modifierID      string
		name            string
		priceDeltaCents int64
	}
	// The order's stored currency is authoritative; the location's current
	// setting is only a fallback for rows written before currency_code was
	// populated. Resolved before the price loop because its Decimals is the
	// exponent every conversion below depends on.
	cur, err := locations.CurrencyFor(ctx, s.pool, locationID)
	if err != nil {
		return nil, fmt.Errorf("resolving currency: %w", err)
	}
	if orderCurrency != nil && *orderCurrency != "" {
		cur.Code = *orderCurrency
	}

	lineUnitCents := make([]int64, len(lines))
	lineModifiers := make([][]resolvedModifier, len(lines))

	for i, line := range lines {
		baseItem := itemCache[line.ItemID]
		// The currency's exponent, not 100 — see CreateOrder for why ×100
		// would charge a yen customer a hundred times over.
		baseCents := int64(math.Round(baseItem.price * float64(money.Scale(cur.Decimals))))
		extra := int64(0)

		if len(line.Modifiers) > 0 {
			mods := make([]resolvedModifier, 0, len(line.Modifiers))
			for _, sel := range line.Modifiers {
				var rm resolvedModifier
				rm.modifierID = sel.ModifierID
				err := tx.QueryRow(ctx, `
					SELECT m.name, m.price_delta_cents
					FROM modifiers m
					JOIN modifier_groups mg ON mg.id = m.modifier_group_id
					WHERE m.id = $1
					  AND mg.item_id = $2
					  AND m.is_active = true
				`, sel.ModifierID, line.ItemID).Scan(&rm.name, &rm.priceDeltaCents)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, fmt.Errorf("%w: modifier %s is not valid for item %s",
						ErrBadModifier, sel.ModifierID, line.ItemID)
				}
				if err != nil {
					return nil, fmt.Errorf("resolving modifier %s: %w", sel.ModifierID, err)
				}
				extra += rm.priceDeltaCents
				mods = append(mods, rm)
			}
			lineModifiers[i] = mods
		}

		lineUnitCents[i] = baseCents + extra
	}

	// --- 6. Compute new order totals ---
	// Read the rate already stored on each item before the lines are deleted.
	// A line that stays keeps that snapshot. A line that is new resolves from
	// the item. Rows written before per-line tax have a NULL rate and fall
	// through to the item, which is the location default when the item has none.
	var subtotalCents int64
	for i, line := range lines {
		subtotalCents += lineUnitCents[i] * int64(line.Quantity)
	}
	kept := map[string]tax.Line{}
	snapRows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (item_id)
		       item_id::text, CAST(tax_rate AS float8), tax_inclusive, COALESCE(tax_label, '')
		FROM order_items
		WHERE order_id = $1 AND tax_rate IS NOT NULL
		ORDER BY item_id, created_at
	`, orderID)
	if err != nil {
		return nil, err
	}
	for snapRows.Next() {
		var itemID, label string
		var percent float64
		var inclusive bool
		if err := snapRows.Scan(&itemID, &percent, &inclusive, &label); err != nil {
			snapRows.Close()
			return nil, err
		}
		kept[itemID] = tax.Line{Percent: percent, Inclusive: inclusive, Label: label}
	}
	snapRows.Close()
	if err := snapRows.Err(); err != nil {
		return nil, err
	}

	priced := make([]linetax.PricedLine, len(lines))
	for i, line := range lines {
		pl := linetax.PricedLine{
			ItemID: line.ItemID,
			Amount: lineUnitCents[i] * int64(line.Quantity),
		}
		if prev, ok := kept[line.ItemID]; ok {
			pl.Override = &prev
		}
		priced[i] = pl
	}
	split, err := linetax.Price(ctx, tx, locationID, priced)
	if err != nil {
		return nil, fmt.Errorf("resolving tax configuration: %w", err)
	}
	taxCents := split.Tax
	totalCents := split.Gross

	// --- 7. Delete existing order_items (cascades to order_item_modifiers) ---
	if _, err := tx.Exec(ctx,
		`DELETE FROM order_items WHERE order_id = $1`, orderID,
	); err != nil {
		return nil, err
	}

	// --- 8. Insert new order_items and order_item_modifiers ---
	for i, line := range lines {
		unitCents := lineUnitCents[i]
		ln := split.Lines[i]
		var orderItemID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO order_items (
				order_id, item_id, quantity, unit_price_cents, total_price_cents,
				special_instructions, course_id,
				tax_rate, tax_inclusive, tax_label, net_cents, tax_cents
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id
		`, orderID, line.ItemID, line.Quantity, unitCents, unitCents*int64(line.Quantity),
			nullStr(line.Notes), nullStr(line.CourseID),
			ln.Percent, ln.Inclusive, ln.Label, ln.Net, ln.Tax,
		).Scan(&orderItemID); err != nil {
			return nil, err
		}

		for _, rm := range lineModifiers[i] {
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_item_modifiers (order_item_id, modifier_id, price_cents_snapshot, name_snapshot)
				VALUES ($1, $2, $3, $4)
			`, orderItemID, rm.modifierID, rm.priceDeltaCents, rm.name,
			); err != nil {
				return nil, err
			}
		}
	}

	// --- 9. Update order totals ---
	if err := linetax.ReplaceLines(ctx, tx, orderID, split.Buckets); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE orders
		SET subtotal_cents = $2,
		    tax_cents      = $3,
		    total_cents    = $4,
		    tax_rate       = $5,
		    tax_inclusive  = $6,
		    tax_mixed      = $7,
		    updated_at     = now()
		WHERE id = $1
	`, orderID, subtotalCents, taxCents, totalCents,
		split.HeaderRate, split.HeaderInclusive, split.Mixed); err != nil {
		return nil, err
	}

	// --- 10. Re-fanout to KDS ---
	// Delete any existing 'fired' tickets (items cascade via FK) then re-insert.
	// Tickets already advanced past 'fired' were blocked at step 2, so all
	// surviving tickets are in the 'fired' state — safe to delete and recreate.
	if _, err := tx.Exec(ctx, `
		DELETE FROM kds_tickets
		WHERE order_id = $1
		  AND status = 'fired'
	`, orderID); err != nil {
		return nil, err
	}

	kdsTicketIDs, fanoutErr := fanoutInsideTx(ctx, tx, orderID)
	if fanoutErr != nil {
		// Non-fatal: enqueue for the background KDS fanout job so the kitchen is
		// never silently missed. Elevate to service role for the queue write.
		qErr := db.WithTxServiceRole(ctx, tx, func() error {
			_, e := tx.Exec(ctx, `
				INSERT INTO kds_fanout_queue (order_id, error_message, retry_count, state)
				VALUES ($1, $2, 0, 'pending')
				ON CONFLICT (order_id) DO UPDATE SET error_message = EXCLUDED.error_message
			`, orderID, fanoutErr.Error())
			return e
		})
		if qErr != nil {
			log.Printf("pos/modify: fanout failed (%v) AND enqueue failed (%v) for order=%s", fanoutErr, qErr, orderID)
		}
		kdsTicketIDs = []string{}
	}

	// --- 11. Audit log (service-role-elevated insert) ---
	beforeJSON, _ := json.Marshal(map[string]any{
		"total_cents": oldTotalCents,
		"item_count":  oldItemCount,
	})
	afterJSON, _ := json.Marshal(map[string]any{
		"total_cents": totalCents,
		"item_count":  len(lines),
	})
	auditErr := db.WithTxServiceRole(ctx, tx, func() error {
		_, e := tx.Exec(ctx, `
			INSERT INTO audit_log (
			    organization_id,
			    actor_type, actor_id,
			    action, entity_type, entity_id,
			    before_state, after_state
			)
			VALUES ($1, 'staff', $2, 'order.modified', 'orders', $3, $4, $5)
		`, orgID, nullStr(actorID), orderID, beforeJSON, afterJSON)
		return e
	})
	if auditErr != nil {
		// Audit failure must not abort the modification — log and continue.
		log.Printf("pos/modify: audit_log insert failed for order=%s: %v", orderID, auditErr)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	minorScale := float64(money.Scale(cur.Decimals))

	return &ModifiedOrder{
		OrderID: orderID,

		SubtotalMinor: subtotalCents,
		TaxMinor:      taxCents,
		TotalMinor:    totalCents,

		Subtotal: float64(subtotalCents) / minorScale,
		Tax:      float64(taxCents) / minorScale,
		Total:    float64(totalCents) / minorScale,

		CurrencyCode:     cur.Code,
		CurrencyDecimals: cur.Decimals,

		TaxRate:      split.HeaderRate,
		TaxInclusive: split.HeaderInclusive,

		KDSTicketIDs: kdsTicketIDs,
	}, nil
}
