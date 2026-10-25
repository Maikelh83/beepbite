package versenbelegd

import (
	"fmt"
	"strings"
	"time"

	"github.com/beepbite/backend/internal/channel"
)

func orderType(ft string) string {
	switch ft {
	case "delivery":
		return "delivery"
	case "dine_in":
		return "dine_in"
	default:
		return "pickup"
	}
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func formatAddress(a channel.Address) string {
	var lines []string
	if a.Company != "" {
		lines = append(lines, a.Company)
	}
	if a.Line1 != "" {
		lines = append(lines, a.Line1)
	}
	if a.Line2 != "" {
		lines = append(lines, a.Line2)
	}
	city := strings.TrimSpace(strings.TrimSpace(a.PostalCode) + " " + strings.TrimSpace(a.City))
	if city != "" {
		lines = append(lines, city)
	}
	return strings.Join(lines, "\n")
}

func splitName(name string) (first, last string) {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "Guest", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.Join(parts[1:], " ")
}

func customerRecordNote(c channel.Customer) string {
	var bits []string
	if c.Phone != "" {
		bits = append(bits, "tel "+c.Phone)
	}
	if c.Company != "" {
		bits = append(bits, c.Company)
	}
	if c.CostCenter != "" {
		bits = append(bits, "kostenplaats "+c.CostCenter)
	}
	bits = append(bits, "bron vers-en-belegd")
	return strings.Join(bits, " · ")
}

func customerNote(o channel.InboundOrder) string {
	var lines []string
	if o.Customer.Company != "" {
		line := o.Customer.Company
		if o.Customer.CostCenter != "" {
			line += " · kostenplaats " + o.Customer.CostCenter
		}
		lines = append(lines, line)
	}
	who := o.Customer.Name
	if o.Customer.Phone != "" {
		who += " · " + o.Customer.Phone
	}
	lines = append(lines, who)
	if o.Notes != "" {
		lines = append(lines, o.Notes)
	}
	return clipRunes(strings.Join(lines, "\n"), 2000)
}

func kitchenNotes(o channel.InboundOrder, zone *time.Location) string {
	var b strings.Builder
	b.WriteString("Bron: Vers & Belegd\n")
	switch o.Fulfillment.Type {
	case "delivery":
		b.WriteString("Bezorgen")
	case "dine_in":
		b.WriteString("Ter plaatse")
	default:
		b.WriteString("Afhalen")
	}
	if !o.Fulfillment.RequestedFor.IsZero() {
		if zone == nil {
			zone = time.UTC
		}
		b.WriteString("\nGewenst: ")
		b.WriteString(o.Fulfillment.RequestedFor.In(zone).Format("2006-01-02 15:04"))
		name := zone.String()
		if name != "" && name != "Local" {
			b.WriteString(" (")
			b.WriteString(name)
			b.WriteString(")")
		}
	}
	if o.Customer.Company != "" {
		b.WriteString("\nBedrijf: ")
		b.WriteString(o.Customer.Company)
	}
	if o.Customer.Phone != "" {
		b.WriteString("\nTel: ")
		b.WriteString(o.Customer.Phone)
	}
	if o.Notes != "" {
		b.WriteString("\n")
		b.WriteString(o.Notes)
	}
	switch o.Payment.Mode {
	case "unpaid":
		if o.Payment.SettleAt == "delivery" {
			b.WriteString("\nNog niet betaald — afrekenen bij bezorging")
		} else {
			b.WriteString("\nNog niet betaald — afrekenen bij afhalen")
		}
	case "on_account":
		b.WriteString("\nOp rekening: ")
		b.WriteString(o.Payment.HouseAccountName)
	}
	return clipRunes(b.String(), 2000)
}

func internalNotes(o channel.InboundOrder, warnings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "bron=vers-en-belegd id=%s\n", o.ExternalID)
	switch o.Payment.Mode {
	case "paid":
		provider := o.Payment.Provider
		if provider == "" {
			provider = "external"
		}
		fmt.Fprintf(&b, "betaling=reeds betaald via %s ref=%s — niet nogmaals incasseren\n", provider, o.Payment.Reference)
	case "on_account":
		fmt.Fprintf(&b, "betaling=op rekening (%s)\n", o.Payment.HouseAccountName)
	default:
		where := "afhalen"
		if o.Payment.SettleAt == "delivery" {
			where = "bezorging"
		}
		fmt.Fprintf(&b, "betaling=nog niet betaald, afrekenen bij %s\n", where)
	}
	for _, w := range warnings {
		b.WriteString(w)
		b.WriteByte('\n')
	}
	return clipRunes(b.String(), 4000)
}

func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
