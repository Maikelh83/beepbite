-- Per-item tax rates, snapshotted onto each sold line.
--
-- tax_rates already allowed several named rates per location, but every sale
-- read only the oldest active row (or the single locations.tax_rate) and
-- applied it to the whole order. A Dutch till needs 9% on food and 21% on
-- alcohol on the same receipt. The item chooses the rate. The line stores
-- the rate it was actually sold at, so a later menu edit cannot restate the
-- sale. order_tax_lines is the receipt: one row per rate, with the base
-- (net), the tax, and the gross.
--
-- Items with tax_rate_id NULL keep the previous rule: the oldest active
-- tax_rates row for the location, otherwise locations.tax_rate /
-- tax_inclusive / tax_label.
--
-- orders.tax_rate stays the single rate when every line shares one. When
-- they do not, tax_mixed is true and tax_rate is 0 — it is not a blended
-- rate. orders.tax_cents is always the sum of the per-rate tax. Readers
-- that need the split use order_tax_lines (or the order_tax_by_rate view),
-- not orders.tax_rate.

ALTER TABLE public.items
    ADD COLUMN tax_rate_id uuid;

ALTER TABLE ONLY public.items
    ADD CONSTRAINT items_tax_rate_id_fkey
    FOREIGN KEY (tax_rate_id) REFERENCES public.tax_rates(id) ON DELETE SET NULL;

COMMENT ON COLUMN public.items.tax_rate_id IS
    'Named rate for this item. NULL means the location default: the oldest active tax_rates row, else locations.tax_rate / tax_inclusive / tax_label. Copied onto the order line at sale time.';

CREATE INDEX idx_items_tax_rate_id ON public.items USING btree (tax_rate_id);

ALTER TABLE public.order_items
    ADD COLUMN tax_rate numeric(5,2),
    ADD COLUMN tax_inclusive boolean,
    ADD COLUMN tax_label text,
    ADD COLUMN net_cents bigint,
    ADD COLUMN tax_cents bigint,
    ADD CONSTRAINT order_items_tax_snapshot_check CHECK (
        (
            tax_rate IS NULL
            AND tax_inclusive IS NULL
            AND net_cents IS NULL
            AND tax_cents IS NULL
        )
        OR (
            tax_rate IS NOT NULL
            AND tax_inclusive IS NOT NULL
            AND net_cents IS NOT NULL
            AND tax_cents IS NOT NULL
            AND tax_rate >= (0)::numeric
            AND tax_rate <= (100)::numeric
        )
    );

COMMENT ON COLUMN public.order_items.tax_rate IS
    'Rate (percent) snapshotted from the item, or from the location default when the item had none. NULL on rows written before per-line tax.';
COMMENT ON COLUMN public.order_items.tax_cents IS
    'Tax portion of this line. With net_cents it sums to the line gross. Allocated so the lines of one rate sum to a single rounding of that rate''s base, not a rounding per line.';
COMMENT ON COLUMN public.order_items.net_cents IS
    'Amount excluding tax. For a tax-inclusive line, net + tax = total_price_cents. For a tax-exclusive line, net = total_price_cents and tax is added on top.';

ALTER TABLE public.orders
    ADD COLUMN tax_mixed boolean DEFAULT false NOT NULL;

COMMENT ON COLUMN public.orders.tax_mixed IS
    'True when the lines were not all taxed at one rate. orders.tax_rate is then 0 and is not a blended rate; the amounts are in order_tax_lines.';
COMMENT ON COLUMN public.orders.tax_rate IS
    'The rate this order was taxed at when every line shared one rate, snapshotted at sale time. When tax_mixed is true this column is 0 and must not be multiplied back through the subtotal. Never recomputed from today''s settings.';

CREATE TABLE public.order_tax_lines (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    order_id uuid NOT NULL,
    tax_label text DEFAULT ''::text NOT NULL,
    tax_rate numeric(5,2) NOT NULL,
    tax_inclusive boolean NOT NULL,
    net_cents bigint NOT NULL,
    tax_cents bigint NOT NULL,
    gross_cents bigint NOT NULL,
    created_at timestamp with time zone DEFAULT timezone('utc'::text, now()) NOT NULL,
    CONSTRAINT order_tax_lines_rate_check CHECK (((tax_rate >= (0)::numeric) AND (tax_rate <= (100)::numeric))),
    CONSTRAINT order_tax_lines_pkey PRIMARY KEY (id),
    CONSTRAINT order_tax_lines_order_rate_key UNIQUE (order_id, tax_label, tax_rate, tax_inclusive)
);

ALTER TABLE ONLY public.order_tax_lines FORCE ROW LEVEL SECURITY;
COMMENT ON TABLE public.order_tax_lines IS
    'One row per tax rate on an order: the base (net), the tax, and the gross. Written at sale time from the line snapshots. This is what a receipt and a VAT return read. Delivery fees and tips are not in these rows.';

ALTER TABLE ONLY public.order_tax_lines
    ADD CONSTRAINT order_tax_lines_order_id_fkey
    FOREIGN KEY (order_id) REFERENCES public.orders(id) ON DELETE CASCADE;

CREATE INDEX idx_order_tax_lines_order_id ON public.order_tax_lines USING btree (order_id);

ALTER TABLE public.order_tax_lines ENABLE ROW LEVEL SECURITY;
CREATE POLICY order_tax_lines_delete ON public.order_tax_lines FOR DELETE USING (public.is_service_role());
CREATE POLICY order_tax_lines_insert ON public.order_tax_lines FOR INSERT WITH CHECK (((order_id IN ( SELECT orders.id
   FROM public.orders
  WHERE (orders.organization_id = public.current_org_id()))) OR public.is_service_role()));
CREATE POLICY order_tax_lines_select ON public.order_tax_lines FOR SELECT USING (((order_id IN ( SELECT orders.id
   FROM public.orders
  WHERE (orders.organization_id = public.current_org_id()))) OR public.is_service_role()));
CREATE POLICY order_tax_lines_update ON public.order_tax_lines FOR UPDATE USING (((order_id IN ( SELECT orders.id
   FROM public.orders
  WHERE (orders.organization_id = public.current_org_id()))) OR public.is_service_role())) WITH CHECK (((order_id IN ( SELECT orders.id
   FROM public.orders
  WHERE (orders.organization_id = public.current_org_id()))) OR public.is_service_role()));

-- Per location, per trading day, per rate. security_invoker so the caller's
-- RLS on orders and order_tax_lines still applies. Days with only pre-snapshot
-- orders do not appear here; those orders still contribute orders.tax_cents
-- as a single figure.
CREATE VIEW public.order_tax_by_rate WITH (security_invoker='on') AS
 SELECT o.organization_id,
    o.location_id,
    o.business_date,
    t.tax_label,
    t.tax_rate,
    t.tax_inclusive,
    sum(t.net_cents) AS net_cents,
    sum(t.tax_cents) AS tax_cents,
    sum(t.gross_cents) AS gross_cents
   FROM (public.order_tax_lines t
     JOIN public.orders o ON ((o.id = t.order_id)))
  GROUP BY o.organization_id, o.location_id, o.business_date, t.tax_label, t.tax_rate, t.tax_inclusive;

COMMENT ON VIEW public.order_tax_by_rate IS
    'VAT base and tax collected, grouped by location, trading day, and rate. Sum of tax_cents across rates is the tax to remit for those orders. Does not include orders written before per-line snapshots.';

GRANT SELECT,INSERT,REFERENCES,DELETE,TRIGGER,TRUNCATE,UPDATE ON TABLE public.order_tax_lines TO service_role;
GRANT SELECT ON TABLE public.order_tax_lines TO marketplace_role;
GRANT SELECT ON TABLE public.order_tax_by_rate TO service_role;

-- Trial databases created a NOSUPERUSER app role outside the migrations.
-- A role that is not the table owner needs an explicit grant or the insert
-- fails even when the RLS policy would allow it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'beepbite_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.order_tax_lines TO beepbite_app;
        GRANT SELECT ON TABLE public.order_tax_by_rate TO beepbite_app;
    END IF;
END $$;
