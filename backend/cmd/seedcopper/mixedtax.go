package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/beepbite/backend/internal/bizday"
	"github.com/beepbite/backend/internal/db"
	"github.com/beepbite/backend/internal/linetax"
)

// seedMixedTax is the Dutch trial menu: food and soft drinks at 9%, wine and
// cocktails at 21%, and one ticket that carries both. Other countries keep a
// single location rate. Re-running updates the two named rates and the item
// pointers; the demo order is inserted once.
func seedMixedTax(s *seeder, c *Ctx) error {
	if s.cfg.Country != "NL" {
		return nil
	}
	inclusive := s.cfg.TaxInclusive()
	var orderNumber string
	err := s.tx(func(tx pgx.Tx) error {
		// The KDS fan-out trigger clears service role for the rest of the
		// transaction. The org id has to be set as well, or the line inserts
		// that follow the order insert are rejected by RLS.
		if err := db.ApplyScope(s.ctx, tx, db.Scope{OrgID: c.OrgID, IsServiceRole: true}); err != nil {
			return err
		}
		low, err := upsertTaxRate(s, tx, c.LocID, "BTW 9%", 9, inclusive)
		if err != nil {
			return err
		}
		high, err := upsertTaxRate(s, tx, c.LocID, "BTW 21%", 21, inclusive)
		if err != nil {
			return err
		}
		var alcohol []string
		for _, name := range []string{"Cocktails", "Wine"} {
			if id := c.Categories[name]; id != "" {
				alcohol = append(alcohol, id)
			}
		}
		if len(alcohol) > 0 {
			if _, err := tx.Exec(s.ctx, `
				UPDATE items SET tax_rate_id = $1
				WHERE location_id = $2 AND category_id::text = ANY($3::text[])
			`, high, c.LocID, alcohol); err != nil {
				return fmt.Errorf("assign 21%%: %w", err)
			}
		}
		if _, err := tx.Exec(s.ctx, `
			UPDATE items SET tax_rate_id = $1
			WHERE location_id = $2
			  AND tax_rate_id IS DISTINCT FROM $3
		`, low, c.LocID, high); err != nil {
			return fmt.Errorf("assign 9%%: %w", err)
		}
		for _, pair := range []struct{ name, sku string }{
			{"Americano", "americano"},
			{"Espresso Martini", "espresso-martini"},
		} {
			id := c.Items[pair.name]
			if id == "" {
				continue
			}
			if _, err := tx.Exec(s.ctx, `
				UPDATE items SET sku = $2
				WHERE id = $1 AND sku IS NULL
			`, id, pair.sku); err != nil {
				return fmt.Errorf("sku %s: %w", pair.sku, err)
			}
		}

		const demoKey = "seed:nl-mixed-btw"
		var existing string
		err = tx.QueryRow(s.ctx, `SELECT order_number FROM orders WHERE idempotency_key = $1`, demoKey).Scan(&existing)
		if err == nil {
			log.Printf("nl tax: demo order already present (%s)", existing)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		foodID, drinkID := c.Items["Americano"], c.Items["Espresso Martini"]
		foodPrice, drinkPrice := c.ItemPrice["Americano"], c.ItemPrice["Espresso Martini"]
		if foodID == "" || drinkID == "" || foodPrice == 0 || drinkPrice == 0 {
			return fmt.Errorf("nl demo needs Americano and Espresso Martini on the menu")
		}
		split, err := linetax.Price(s.ctx, tx, c.LocID, []linetax.PricedLine{
			{ItemID: foodID, Amount: foodPrice},
			{ItemID: drinkID, Amount: drinkPrice},
		})
		if err != nil {
			return err
		}
		businessDate := bizday.Date(time.Now(), bizday.Zone(s.cfg.Timezone))
		var orderID string
		if err := tx.QueryRow(s.ctx, `
			INSERT INTO orders (
				organization_id, location_id, order_number,
				order_type, fulfillment_type, status,
				subtotal_cents, tax_cents, total_cents,
				tax_rate, tax_inclusive, tax_mixed, currency_code,
				business_date, idempotency_key,
				notes, kitchen_notes
			) VALUES (
				$1, $2, 'BTW0001',
				'pickup', 'collection', 'confirmed',
				$3, $4, $5,
				$6, $7, $8, $9,
				$10::date, $11,
				$12, $12
			) RETURNING id::text
		`, c.OrgID, c.LocID,
			foodPrice+drinkPrice, split.Tax, split.Gross,
			split.HeaderRate, split.HeaderInclusive, split.Mixed, s.cfg.Currency,
			businessDate, demoKey,
			"Proefbon gemengde BTW: Americano 9% en Espresso Martini 21%.",
		).Scan(&orderID); err != nil {
			return fmt.Errorf("demo order: %w", err)
		}
		lines := []struct {
			id, name string
			price    int64
		}{
			{foodID, "Americano", foodPrice},
			{drinkID, "Espresso Martini", drinkPrice},
		}
		for i, ln := range lines {
			rated := split.Lines[i]
			if _, err := tx.Exec(s.ctx, `
				INSERT INTO order_items (
					order_id, item_id, quantity, unit_price_cents, total_price_cents,
					tax_rate, tax_inclusive, tax_label, net_cents, tax_cents
				) VALUES ($1, $2, 1, $3, $3, $4, $5, $6, $7, $8)
			`, orderID, ln.id, ln.price,
				rated.Percent, rated.Inclusive, rated.Label, rated.Net, rated.Tax); err != nil {
				return fmt.Errorf("demo line %s: %w", ln.name, err)
			}
		}
		if err := linetax.ReplaceLines(s.ctx, tx, orderID, split.Buckets); err != nil {
			return err
		}
		orderNumber = "BTW0001"
		log.Printf("nl tax: 9%% + 21%% on one ticket %s (tax %d, total %d %s)",
			orderNumber, split.Tax, split.Gross, s.cfg.Currency)
		return nil
	})
	return err
}

func upsertTaxRate(s *seeder, tx pgx.Tx, locationID, name string, rate float64, inclusive bool) (string, error) {
	var id string
	err := tx.QueryRow(s.ctx, `
		INSERT INTO tax_rates (location_id, name, rate, is_inclusive, is_active, created_at)
		VALUES ($1, $2, $3, $4, true, clock_timestamp())
		ON CONFLICT (location_id, name) DO UPDATE
		SET rate = EXCLUDED.rate,
		    is_inclusive = EXCLUDED.is_inclusive,
		    is_active = true
		RETURNING id::text
	`, locationID, name, rate, inclusive).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("tax rate %s: %w", name, err)
	}
	return id, nil
}
