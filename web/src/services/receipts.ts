// receipts.js — service layer for the receipt reprint feature (Wave 24).
// Wraps the GET /orders/{order_id}/receipt backend endpoint.

import { api } from '@/lib/api-client';

export interface ReceiptModifier {
  name: string;
  price_cents_snapshot: number;
}

export interface ReceiptLineItem {
  order_item_id: string;
  item_name: string;
  quantity: number;
  unit_price_cents: number;
  total_price_cents: number;
  modifiers: ReceiptModifier[];
}

export interface ReceiptPayment {
  payment_id: string;
  method: string;
  amount_paid_cents: number;
  tip_amount_cents: number;
  change_given_cents: number;
  payment_reference?: string | null;
  paid_at: string;
}

export interface ReceiptTaxLine {
  label: string;
  /** Percent, e.g. 9 or 21. */
  rate: number;
  inclusive: boolean;
  net_cents: number;
  tax_cents: number;
  gross_cents: number;
}

export interface Receipt {
  store_name: string;
  store_address?: string | null;
  order_id: string;
  order_number: string;
  /** ISO-8601 timestamptz */
  created_at: string;
  line_items: ReceiptLineItem[];
  subtotal_cents: number;
  tax_cents: number;
  tax_lines?: ReceiptTaxLine[];
  tip_cents: number;
  total_cents: number;
  currency_code: string;
  payments: ReceiptPayment[];
  fiscal_receipt_number?: string | null;
}

export interface ReceiptTaxRow {
  label: string;
  cents: number;
}

/** Base + tax for each rate. A receipt with no snapshot falls back to one tax row. */
export function receiptTaxRows(receipt: Pick<Receipt, 'tax_lines' | 'tax_cents'>): ReceiptTaxRow[] {
  const lines = receipt.tax_lines ?? [];
  if (lines.length === 0) {
    return [{ label: 'Tax', cents: receipt.tax_cents }];
  }
  const rows: ReceiptTaxRow[] = [];
  for (const ln of lines) {
    const name = ln.label || 'Tax';
    const rate = Number.isInteger(ln.rate) ? String(ln.rate) : String(ln.rate);
    const titled = name.includes('%') ? name : `${name} ${rate}%`;
    rows.push({ label: `${titled} base`, cents: ln.net_cents });
    rows.push({ label: ln.inclusive ? `${titled} (incl.)` : titled, cents: ln.tax_cents });
  }
  return rows;
}

/**
 * Fetch the structured receipt for a past order.
 *
 * @param orderId  - UUID of the order to reprint.
 */
export async function fetchReceipt(orderId: string) {
  if (!orderId) {
    return { data: null, error: { message: 'orderId is required' } };
  }
  return api.request<Receipt>('GET', `/orders/${encodeURIComponent(orderId)}/receipt`);
}
