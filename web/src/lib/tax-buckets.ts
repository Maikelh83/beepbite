// Per-rate tax for a cart. Matches backend/internal/tax.SplitLines:
// one rounding per rate, then the cents are shared back across the lines.
// amountCents is the extended menu price (gross when inclusive, net when not).

export interface TaxBucketInput {
  amountCents: number;
  ratePercent: number;
  inclusive: boolean;
  label: string;
}

export interface TaxBucket {
  label: string;
  rate: number;
  inclusive: boolean;
  netCents: number;
  taxCents: number;
  grossCents: number;
}

export interface TaxSplit {
  buckets: TaxBucket[];
  netCents: number;
  taxCents: number;
  grossCents: number;
  mixed: boolean;
}

function rateBp(pct: number): number {
  const n = Number(pct) || 0;
  if (n < 0) return -Math.trunc(-n * 100 + 0.5);
  return Math.trunc(n * 100 + 0.5);
}

function divRound(n: number, d: number): number {
  if (d === 0) return 0;
  if (d < 0) {
    n = -n;
    d = -d;
  }
  if (n >= 0) return Math.trunc((n + Math.trunc(d / 2)) / d);
  return -Math.trunc((-n + Math.trunc(d / 2)) / d);
}

function compute(amount: number, bp: number, inclusive: boolean) {
  if (bp <= 0) return { net: amount, tax: 0, gross: amount };
  if (inclusive) {
    const tax = divRound(amount * bp, 10000 + bp);
    return { net: amount - tax, tax, gross: amount };
  }
  const tax = divRound(amount * bp, 10000);
  return { net: amount, tax, gross: amount + tax };
}

function allocate(total: number, weights: number[]): number[] {
  const out = weights.map(() => 0);
  if (weights.length === 0 || total === 0) return out;
  let sign = 1;
  let leftTotal = total;
  if (leftTotal < 0) {
    sign = -1;
    leftTotal = -leftTotal;
  }
  const mags = weights.map((w) => (w < 0 ? -w : w));
  const sum = mags.reduce((s, w) => s + w, 0);
  if (sum === 0) {
    out[0] = sign * leftTotal;
    return out;
  }
  const fracs = mags.map((m, i) => {
    const prod = leftTotal * m;
    out[i] = Math.trunc(prod / sum);
    return { i, rem: prod % sum };
  });
  let assigned = out.reduce((s, n) => s + n, 0);
  let left = leftTotal - assigned;
  fracs.sort((a, b) => (b.rem - a.rem) || (a.i - b.i));
  for (let k = 0; left > 0 && k < fracs.length; k++) {
    out[fracs[k].i] += 1;
    left -= 1;
  }
  if (sign < 0) return out.map((n) => -n);
  return out;
}

export function splitTax(lines: TaxBucketInput[]): TaxSplit {
  const result: TaxSplit = {
    buckets: [],
    netCents: 0,
    taxCents: 0,
    grossCents: 0,
    mixed: false,
  };
  if (lines.length === 0) return result;

  type Group = {
    bp: number;
    percent: number;
    inclusive: boolean;
    label: string;
    sum: number;
    idxs: number[];
  };
  const groups: Group[] = [];
  const indexOf = new Map<string, number>();
  lines.forEach((ln, i) => {
    const label = ln.label || 'Tax';
    const bp = rateBp(ln.ratePercent);
    const key = `${bp}|${ln.inclusive ? 1 : 0}|${label}`;
    let gi = indexOf.get(key);
    if (gi === undefined) {
      gi = groups.length;
      indexOf.set(key, gi);
      groups.push({
        bp,
        percent: bp / 100,
        inclusive: ln.inclusive,
        label,
        sum: 0,
        idxs: [],
      });
    }
    groups[gi].sum += ln.amountCents;
    groups[gi].idxs.push(i);
  });

  for (const g of groups) {
    const res = compute(g.sum, g.bp, g.inclusive);
    const weights = g.idxs.map((idx) => lines[idx].amountCents);
    allocate(res.tax, weights);
    result.buckets.push({
      label: g.label,
      rate: g.percent,
      inclusive: g.inclusive,
      netCents: res.net,
      taxCents: res.tax,
      grossCents: res.gross,
    });
    result.netCents += res.net;
    result.taxCents += res.tax;
    result.grossCents += res.gross;
  }
  result.mixed = result.buckets.length > 1;
  return result;
}
