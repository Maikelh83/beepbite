package tax

import "sort"

// Line is one sold line before tax is grouped. Amount is the extended menu
// price in minor units: the gross when Inclusive is true, the net when it is
// false. Percent is the decimal(5,2) rate, for example 9 or 21.
type Line struct {
	Amount    int64
	Percent   float64
	Inclusive bool
	Label     string
}

// LineResult is the tax snapshot stored on one order line. Net + Tax = Gross.
// Lines that share a rate are allocated so their taxes sum to one rounding of
// the combined base, which is what a VAT return does per rate.
type LineResult struct {
	Percent   float64
	Inclusive bool
	Label     string
	Net       int64
	Tax       int64
	Gross     int64
}

// BreakdownLine is one rate on a receipt or in order_tax_lines.
type BreakdownLine struct {
	Label      string  `json:"label"`
	Rate       float64 `json:"rate"`
	Inclusive  bool    `json:"inclusive"`
	NetCents   int64   `json:"net_cents"`
	TaxCents   int64   `json:"tax_cents"`
	GrossCents int64   `json:"gross_cents"`
}

// Split is the tax on a set of lines.
//
// Gross is what the customer pays for the lines (tips and delivery are not
// included; those are not part of the rated base). Tax is the sum of the
// per-rate tax. When Mixed is false, HeaderRate is that one rate. When Mixed
// is true, HeaderRate is 0 and must not be treated as a blended rate.
type Split struct {
	Lines           []LineResult
	Buckets         []BreakdownLine
	Net             int64
	Tax             int64
	Gross           int64
	Mixed           bool
	HeaderRate      float64
	HeaderInclusive bool
	HeaderLabel     string
}

type rateKey struct {
	bp        BasisPoints
	inclusive bool
	label     string
}

// SplitLines groups lines by rate, inclusive flag, and label, rounds tax once
// per group, and allocates that tax back onto the lines.
//
// A single rate therefore matches Compute on the summed amount, including the
// cent that per-line rounding would have drifted. Two rates each get their
// own rounding. An empty input charges nothing.
func SplitLines(lines []Line) Split {
	out := Split{
		Lines:           make([]LineResult, len(lines)),
		HeaderInclusive: true,
	}
	if len(lines) == 0 {
		return out
	}

	type group struct {
		key     rateKey
		percent float64
		sum     int64
		idxs    []int
	}
	var groups []group
	indexOf := map[rateKey]int{}

	for i, ln := range lines {
		label := ln.Label
		if label == "" {
			label = DefaultLabel
		}
		bp := RateFromPercent(ln.Percent)
		key := rateKey{bp: bp, inclusive: ln.Inclusive, label: label}
		gi, ok := indexOf[key]
		if !ok {
			gi = len(groups)
			indexOf[key] = gi
			groups = append(groups, group{
				key:     key,
				percent: bp.Percent(),
			})
		}
		groups[gi].sum += ln.Amount
		groups[gi].idxs = append(groups[gi].idxs, i)
	}

	for _, g := range groups {
		res := Compute(g.sum, g.key.bp, g.key.inclusive)
		weights := make([]int64, len(g.idxs))
		for j, idx := range g.idxs {
			weights[j] = lines[idx].Amount
		}
		shares := allocate(res.Tax, weights)
		for j, idx := range g.idxs {
			taxAmt := shares[j]
			amount := lines[idx].Amount
			var net, gross int64
			if g.key.inclusive {
				gross = amount
				net = amount - taxAmt
			} else {
				net = amount
				gross = amount + taxAmt
			}
			out.Lines[idx] = LineResult{
				Percent:   g.percent,
				Inclusive: g.key.inclusive,
				Label:     g.key.label,
				Net:       net,
				Tax:       taxAmt,
				Gross:     gross,
			}
		}
		out.Buckets = append(out.Buckets, BreakdownLine{
			Label:      g.key.label,
			Rate:       g.percent,
			Inclusive:  g.key.inclusive,
			NetCents:   res.Net,
			TaxCents:   res.Tax,
			GrossCents: res.Gross,
		})
		out.Net += res.Net
		out.Tax += res.Tax
		out.Gross += res.Gross
	}

	if len(out.Buckets) == 1 {
		b := out.Buckets[0]
		out.HeaderRate = b.Rate
		out.HeaderInclusive = b.Inclusive
		out.HeaderLabel = b.Label
		return out
	}
	out.Mixed = true
	out.HeaderRate = 0
	out.HeaderInclusive = true
	for _, b := range out.Buckets {
		if !b.Inclusive {
			out.HeaderInclusive = false
			break
		}
	}
	return out
}

// allocate spreads total across weights so the shares sum to total.
// Weights may be negative; the magnitude is used, and the sign of total is
// applied to every share. A zero weight sum puts the whole total on the
// first line.
func allocate(total int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	if len(weights) == 0 || total == 0 {
		return out
	}
	sign := int64(1)
	if total < 0 {
		sign = -1
		total = -total
	}
	var sum int64
	mags := make([]int64, len(weights))
	for i, w := range weights {
		if w < 0 {
			w = -w
		}
		mags[i] = w
		sum += w
	}
	if sum == 0 {
		out[0] = sign * total
		return out
	}

	type frac struct {
		i   int
		rem int64
	}
	fracs := make([]frac, len(weights))
	var assigned int64
	for i, m := range mags {
		prod := total * m
		out[i] = prod / sum
		assigned += out[i]
		fracs[i] = frac{i: i, rem: prod % sum}
	}
	left := total - assigned
	sort.SliceStable(fracs, func(a, b int) bool {
		if fracs[a].rem != fracs[b].rem {
			return fracs[a].rem > fracs[b].rem
		}
		return fracs[a].i < fracs[b].i
	})
	for k := 0; left > 0 && k < len(fracs); k++ {
		out[fracs[k].i]++
		left--
	}
	if sign < 0 {
		for i := range out {
			out[i] = -out[i]
		}
	}
	return out
}
