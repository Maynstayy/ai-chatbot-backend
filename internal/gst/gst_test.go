package gst

import "testing"

func TestRateSlabs(t *testing.T) {
	cases := []struct {
		price float64
		want  float64
	}{
		{0, 0}, {400, 0}, {999.99, 0}, {1000, 0},
		{1000.01, 0.05}, {1001, 0.05}, {2000, 0.05}, {7499.99, 0.05}, {7500, 0.05},
		{7500.01, 0.18}, {7501, 0.18}, {11000, 0.18},
	}
	for _, c := range cases {
		if got := Rate(c.price); got != c.want {
			t.Errorf("Rate(%v) = %v, want %v", c.price, got, c.want)
		}
	}
}

func TestTaxPerNight(t *testing.T) {
	if got := TaxPerNight(2000); got != 100 {
		t.Errorf("2000 -> %v, want 100", got)
	}
	if got := TaxPerNight(1000); got != 0 {
		t.Errorf("1000 -> %v, want 0 (exempt)", got)
	}
	if got := TaxPerNight(8000); got != 1440 {
		t.Errorf("8000 -> %v, want 1440 (18%%)", got)
	}
}

func TestForStayExample(t *testing.T) { // Rs 2,000 room, 2 nights, 1 room
	b := ForStay([]float64{2000, 2000}, 1)
	if b.Tax != 200 || b.CGST != 100 || b.SGST != 100 {
		t.Fatalf("got %+v, want tax 200 = 100 CGST + 100 SGST", b)
	}
}

func TestForStayMultipleRooms(t *testing.T) {
	b := ForStay([]float64{2000, 2000}, 3)
	if b.Tax != 600 {
		t.Fatalf("tax = %v, want 600", b.Tax)
	}
}

func TestForStayEachNightIsSlabbedOnItsOwn(t *testing.T) {
	b := ForStay([]float64{7000, 8000}, 1) // 5% of 7000 + 18% of 8000
	if b.Tax != 1790 {
		t.Fatalf("tax = %v, want 1790", b.Tax)
	}
}

func TestForStayExemptStayHasNoTax(t *testing.T) {
	if b := ForStay([]float64{700, 700, 700}, 2); b.Tax != 0 || b.CGST != 0 || b.SGST != 0 {
		t.Fatalf("got %+v, want zero", b)
	}
}

func TestForStaySplitAlwaysAddsUp(t *testing.T) {
	for _, price := range []float64{1001, 1234.5, 1999.99, 3333.33, 7499.99, 8123.45} {
		b := ForStay([]float64{price, price, price}, 1)
		if sum := b.CGST + b.SGST; sum < b.Tax-0.001 || sum > b.Tax+0.001 {
			t.Errorf("price %v: CGST %v + SGST %v != tax %v", price, b.CGST, b.SGST, b.Tax)
		}
	}
}
