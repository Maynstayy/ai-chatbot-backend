package chatbot

import (
	"context"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
)

func TestPrimaryImage(t *testing.T) {
	cases := []struct {
		name  string
		hotel hotels.Hotel
		want  string
	}{
		{"thumbnail first, unchanged", hotels.Hotel{ThumbnailURL: "/uploads/hotels/a.jpg", Images: []string{"/uploads/hotels/b.jpg"}}, "/uploads/hotels/a.jpg"},
		{"first gallery image without a thumbnail", hotels.Hotel{Images: []string{"/uploads/hotels/b.jpg", "/uploads/hotels/c.jpg"}}, "/uploads/hotels/b.jpg"},
		{"absolute https kept as is", hotels.Hotel{ThumbnailURL: "https://cdn.example.com/x.png?w=800"}, "https://cdn.example.com/x.png?w=800"},
		{"unsafe thumbnail skipped for a safe image", hotels.Hotel{ThumbnailURL: "javascript:alert(1)", Images: []string{"data:image/png;base64,xx", "//evil.example/x.jpg", "/uploads/hotels/ok.jpg"}}, "/uploads/hotels/ok.jpg"},
		{"nothing usable", hotels.Hotel{ThumbnailURL: "  ", Images: []string{"", "not a url"}}, ""},
	}
	for _, c := range cases {
		if got := primaryImage(c.hotel); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The card a search returns carries the hotel's own primary photo, matched
// to the right hotel.
func TestSearchCardsCarryEachHotelsOwnImage(t *testing.T) {
	f := newFixture()
	a, b := haridwarHotel, haridwarHotel
	a.HotelID, a.Name, a.ThumbnailURL = 11, "Ganga View Residency", "/uploads/hotels/ganga.jpg"
	b.HotelID, b.Name, b.ThumbnailURL, b.Images = 13, "Har Ki Pauri Inn", "", []string{"/uploads/hotels/pauri-1.jpeg", "/uploads/hotels/pauri-2.jpeg"}
	f.hotels.list = []hotels.Hotel{a, b}
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{11: "/uploads/hotels/ganga.jpg", 13: "/uploads/hotels/pauri-1.jpeg"}
	if len(out.Hotels) != 2 {
		t.Fatalf("hotels: %+v", out.Hotels)
	}
	for _, h := range out.Hotels {
		if h.ThumbnailURL != want[h.HotelID] {
			t.Errorf("hotel %d (%s): image %q, want %q", h.HotelID, h.Name, h.ThumbnailURL, want[h.HotelID])
		}
	}
}
