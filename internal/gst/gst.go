// Package gst works out the GST on hotel accommodation. The slab follows the
// room tariff actually charged per room per night (after discounts):
//
//	up to Rs 1,000        exempt (0%)
//	Rs 1,001 - Rs 7,500   5%  (2.5% CGST + 2.5% SGST), no input tax credit
//	above Rs 7,500        18% (9% CGST + 9% SGST)
//
// Every night of a stay is slabbed on its own, so a seasonal rate that pushes
// one night over a threshold only changes that night's tax.
package gst

import "math"

const (
	ExemptUpTo  = 1000.0 // nightly tariff up to this carries no GST
	ReducedUpTo = 7500.0 // nightly tariff up to this carries 5%, above it 18%
)

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Rate is the GST rate (0, 0.05 or 0.18) for one room-night at this tariff.
func Rate(nightly float64) float64 {
	n := round2(nightly)
	switch {
	case n <= ExemptUpTo:
		return 0
	case n <= ReducedUpTo:
		return 0.05
	default:
		return 0.18
	}
}

// TaxPerNight is the GST on one room-night at this tariff, for display.
func TaxPerNight(nightly float64) float64 { return round2(nightly * Rate(nightly)) }

// Breakdown is the GST on a stay. CGST + SGST always add up to Tax exactly.
type Breakdown struct {
	Tax  float64
	CGST float64
	SGST float64
}

// ForStay returns the GST for a stay: nightly holds what one room is charged
// for each night (after any discount) and rooms is how many rooms were booked.
func ForStay(nightly []float64, rooms int) Breakdown {
	if rooms < 1 {
		rooms = 1
	}
	var total float64
	for _, n := range nightly {
		total += n * Rate(n)
	}
	total = round2(total * float64(rooms))
	cgst := round2(total / 2)
	return Breakdown{Tax: total, CGST: cgst, SGST: round2(total - cgst)}
}
