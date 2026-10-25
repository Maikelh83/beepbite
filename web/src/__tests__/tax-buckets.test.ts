import { describe, expect, it } from 'vitest';
import { splitTax } from '../lib/tax-buckets';

describe('splitTax', () => {
  it('rounds once per rate, not once per line', () => {
    const got = splitTax([
      { amountCents: 100, ratePercent: 9, inclusive: true, label: 'BTW' },
      { amountCents: 100, ratePercent: 9, inclusive: true, label: 'BTW' },
      { amountCents: 100, ratePercent: 9, inclusive: true, label: 'BTW' },
    ]);
    expect(got.mixed).toBe(false);
    expect(got.taxCents).toBe(25);
    expect(got.grossCents).toBe(300);
    expect(got.netCents).toBe(275);
  });

  it('splits 9% food and 21% alcohol on one ticket', () => {
    const got = splitTax([
      { amountCents: 2800, ratePercent: 9, inclusive: true, label: 'BTW 9%' },
      { amountCents: 11500, ratePercent: 21, inclusive: true, label: 'BTW 21%' },
    ]);
    expect(got.mixed).toBe(true);
    expect(got.grossCents).toBe(14300);
    expect(got.buckets[0]).toMatchObject({ netCents: 2569, taxCents: 231, grossCents: 2800 });
    expect(got.buckets[1]).toMatchObject({ netCents: 9504, taxCents: 1996, grossCents: 11500 });
  });
});
