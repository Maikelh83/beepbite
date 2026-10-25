package tax

import "testing"

func TestSplitLines_SingleRateMatchesOneRounding(t *testing.T) {
	// Three inclusive lines of 100 at 9%. Rounding each line is 8+8+8 = 24.
	// Rounding the combined 300 once is 25. The receipt must use 25, and the
	// line taxes must sum to it.
	lines := []Line{
		{Amount: 100, Percent: 9, Inclusive: true, Label: "BTW"},
		{Amount: 100, Percent: 9, Inclusive: true, Label: "BTW"},
		{Amount: 100, Percent: 9, Inclusive: true, Label: "BTW"},
	}
	got := SplitLines(lines)
	want := Compute(300, RateFromPercent(9), true)
	if got.Mixed {
		t.Fatal("one rate is not mixed")
	}
	if got.Tax != want.Tax || got.Net != want.Net || got.Gross != want.Gross {
		t.Fatalf("group = %+v, Compute(300) = %+v", got, want)
	}
	if got.HeaderRate != 9 || !got.HeaderInclusive || got.HeaderLabel != "BTW" {
		t.Fatalf("header = %v %v %q", got.HeaderRate, got.HeaderInclusive, got.HeaderLabel)
	}
	var sum int64
	for _, ln := range got.Lines {
		sum += ln.Tax
		if ln.Net+ln.Tax != ln.Gross {
			t.Fatalf("line does not reconcile: %+v", ln)
		}
	}
	if sum != want.Tax {
		t.Fatalf("line taxes %d, want %d", sum, want.Tax)
	}
}

func TestSplitLines_NineAndTwentyOneInclusive(t *testing.T) {
	// €28.00 at 9% and €115.00 at 21%, both already in the price.
	got := SplitLines([]Line{
		{Amount: 2800, Percent: 9, Inclusive: true, Label: "BTW 9%"},
		{Amount: 11500, Percent: 21, Inclusive: true, Label: "BTW 21%"},
	})
	if !got.Mixed {
		t.Fatal("two rates must be mixed")
	}
	if got.HeaderRate != 0 {
		t.Fatalf("header rate = %v, want 0 (not a blended rate)", got.HeaderRate)
	}
	if !got.HeaderInclusive {
		t.Fatal("both rates are inclusive")
	}
	if got.Gross != 14300 {
		t.Fatalf("gross = %d, want 14300", got.Gross)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("buckets = %d", len(got.Buckets))
	}
	food, drink := got.Buckets[0], got.Buckets[1]
	if food.TaxCents != 231 || food.NetCents != 2569 || food.GrossCents != 2800 {
		t.Fatalf("9%% = %+v, want net 2569 tax 231 gross 2800", food)
	}
	if drink.TaxCents != 1996 || drink.NetCents != 9504 || drink.GrossCents != 11500 {
		t.Fatalf("21%% = %+v, want net 9504 tax 1996 gross 11500", drink)
	}
	if got.Tax != 231+1996 {
		t.Fatalf("tax = %d", got.Tax)
	}
	if got.Lines[0].Tax != 231 || got.Lines[1].Tax != 1996 {
		t.Fatalf("line taxes = %d, %d", got.Lines[0].Tax, got.Lines[1].Tax)
	}
}

func TestSplitLines_ExclusiveAddsOnTop(t *testing.T) {
	got := SplitLines([]Line{
		{Amount: 1000, Percent: 21, Inclusive: false, Label: "Sales Tax"},
	})
	if got.Tax != 210 || got.Gross != 1210 || got.Net != 1000 {
		t.Fatalf("got %+v", got)
	}
	if got.Lines[0].Gross != 1210 {
		t.Fatalf("line gross = %d", got.Lines[0].Gross)
	}
}

func TestSplitLines_ZeroRateChargesNothing(t *testing.T) {
	got := SplitLines([]Line{{Amount: 500, Percent: 0, Inclusive: true}})
	if got.Tax != 0 || got.Gross != 500 || got.Lines[0].Label != DefaultLabel {
		t.Fatalf("got %+v line %+v", got, got.Lines[0])
	}
}

func TestSplitLines_Empty(t *testing.T) {
	got := SplitLines(nil)
	if got.Tax != 0 || got.Gross != 0 || got.Mixed || len(got.Buckets) != 0 {
		t.Fatalf("got %+v", got)
	}
}
