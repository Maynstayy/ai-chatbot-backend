package rooms

import (
	"testing"
	"time"
)

func TestDiscountJudgedOnCheckIn(t *testing.T) {
	// the form the database driver returns for a DATE/DATETIME column
	from, to := "2026-10-03T00:00:00Z", "2026-12-31T00:00:00Z"
	plan := RatePlan{PricePerNight: 2000, DiscountType: "percentage", DiscountValue: 15, DiscountValidFrom: &from, DiscountValidTo: &to}
	day := func(s string) time.Time { d, _ := time.Parse(dateLayout, s); return d }

	tests := []struct {
		checkIn string
		want    float64
	}{
		{"2026-10-02", 2000}, // day before the window opens
		{"2026-10-03", 1700}, // first day, inclusive
		{"2026-11-15", 1700},
		{"2026-12-31", 1700}, // last day, inclusive
		{"2027-01-01", 2000}, // day after it closes
		{"2027-02-10", 2000},
	}
	for _, tc := range tests {
		if got := EffectivePriceForBooking(plan, day(tc.checkIn)); got != tc.want {
			t.Errorf("check-in %s: charged %.2f, want %.2f", tc.checkIn, got, tc.want)
		}
		shown := plan
		ApplyPublicPricingOn(&shown, day(tc.checkIn))
		if shown.PricePerNight != tc.want {
			t.Errorf("check-in %s: displayed %.2f, want %.2f", tc.checkIn, shown.PricePerNight, tc.want)
		}
	}
}
