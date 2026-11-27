package chatbot

// Answers that come straight from verified data, without the model:
//
//   - what is near a hotel, and how far a station or airport is: from the
//     hotel's own coordinates and the admin-maintained landmarks of its city,
//     as straight-line distances (never routes or road distances);
//   - directions: the hotel's address, with the hotel page for the map;
//   - starting prices: from the live listings.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/landmarks"
)

// LandmarkSource is satisfied by landmarks.Service.
type LandmarkSource interface {
	ListByCity(ctx context.Context, city string) ([]landmarks.Landmark, error)
}

// SetLandmarks enables nearby-place answers. Without it, distance questions
// are answered with the hotel's address only.
func (s *Service) SetLandmarks(l LandmarkSource) { s.landmarks = l }

var (
	nearbyAsk     = regexp.MustCompile(`(?i)\b(near|nearby|nearest|distance|how far|far from|km|kilomet\w*|close to|walking distance|attractions?|landmarks?|around the hotel|places around|station|airport)\b`)
	directionsAsk = regexp.MustCompile(`(?i)\b(directions?|how (do i|to|can i) (get|reach|go)|route|address|located|location|where is|where's)\b`)
	stationAsk    = regexp.MustCompile(`(?i)\b(railway|train|station|junction)\b`)
	airportAsk    = regexp.MustCompile(`(?i)\bairport\b`)
	stationName   = regexp.MustCompile(`(?i)\b(railway|station|junction|jn)\b`)
	airportName   = regexp.MustCompile(`(?i)\b(airport|aerodrome)\b`)

	priceAsk = regexp.MustCompile(`(?i)\b(prices?|pricing|rates?|tariffs?|cost|costs|charges|how much|kitna|kitne|kimat|keemat|kiraya)\b`)
)

const maxNearbyKm = 25

// distanceKm is the great-circle (straight-line) distance.
func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := rad(lat2-lat1), rad(lon2-lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

func formatKm(km float64) string {
	if km < 1 {
		return fmt.Sprintf("%d m", int(math.Round(km*10))*100)
	}
	return fmt.Sprintf("%.1f km", km)
}

// locationReply answers directions and nearby questions for listed hotels,
// or returns nil when the message asks neither.
func (s *Service) locationReply(ctx context.Context, msg string, ids []int64) *directReply {
	wantsNearby, wantsDirections := nearbyAsk.MatchString(msg), directionsAsk.MatchString(msg)
	if !wantsNearby && !wantsDirections {
		return nil
	}
	var parts []string
	for i, id := range ids {
		if i == 2 {
			break
		}
		h, err := s.hotels.GetHotelByID(ctx, id)
		if err != nil || h == nil || !h.IsActive || h.IsDeleted {
			continue
		}
		address := strings.TrimSpace(h.Address)
		if address == "" {
			address = strings.TrimSpace(strings.Join(nonEmpty(h.Location, h.City), ", "))
		}
		if !wantsNearby {
			parts = append(parts, fmt.Sprintf("%s is at %s. Open the hotel's page for the map and directions.", h.Name, address))
			continue
		}
		parts = append(parts, s.nearbyFor(ctx, h, msg, address))
	}
	if len(parts) == 0 {
		return nil
	}
	return &directReply{text: strings.Join(parts, "\n\n"), intent: IntentHotelInfo}
}

func (s *Service) nearbyFor(ctx context.Context, h *hotels.Hotel, msg, address string) string {
	cannot := fmt.Sprintf("I can't verify distances for %s yet. Its address is %s; the hotel's page has the map.", h.Name, address)
	if h.Latitude == nil || h.Longitude == nil || s.landmarks == nil {
		return cannot
	}
	list, err := s.landmarks.ListByCity(ctx, h.City)
	if err != nil {
		return cannot
	}
	kind := ""
	switch {
	case stationAsk.MatchString(msg):
		kind = "station"
	case airportAsk.MatchString(msg):
		kind = "airport"
	}
	type near struct {
		name string
		km   float64
	}
	var found []near
	for _, l := range list {
		if !l.IsActive || (l.Latitude == 0 && l.Longitude == 0) {
			continue
		}
		label := l.Name + " " + l.Category
		if (kind == "station" && !stationName.MatchString(label)) || (kind == "airport" && !airportName.MatchString(label)) {
			continue
		}
		km := distanceKm(*h.Latitude, *h.Longitude, l.Latitude, l.Longitude)
		if km <= maxNearbyKm {
			found = append(found, near{l.Name, km})
		}
	}
	if len(found) == 0 {
		if kind != "" {
			what := "railway station"
			if kind == "airport" {
				what = "airport"
			}
			return fmt.Sprintf("No %s is listed near %s, so I can't verify that distance. Its address is %s; the hotel's page has the map.", what, h.Name, address)
		}
		return cannot
	}
	sort.Slice(found, func(i, j int) bool { return found[i].km < found[j].km })
	if len(found) > 4 {
		found = found[:4]
	}
	var items []string
	for _, f := range found {
		items = append(items, f.name+" about "+formatKm(f.km))
	}
	return fmt.Sprintf("Near %s (straight-line distance): %s. Road distance can be longer.", h.Name, strings.Join(items, ", "))
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

// --- prices from the live listings ------------------------------------------

// hotelPriceReply gives the starting price of the looked-up hotels.
func hotelPriceReply(cards []HotelCard) string {
	if len(cards) == 0 {
		return ""
	}
	var items, itemsHi []string
	for i, c := range cards {
		if i == 3 {
			break
		}
		items = append(items, fmt.Sprintf("%s from ₹%s", c.Name, inr(c.PricePerNight)))
		itemsHi = append(itemsHi, fmt.Sprintf("%s ₹%s se", c.Name, inr(c.PricePerNight)))
	}
	return fmt.Sprintf("Starting prices per room per night, before GST: %s. Live room prices depend on your dates and room type; choose dates on the hotel's page to see them.", strings.Join(items, ", "))
}

// cityPrices is the lowest listed nightly price per city, cities ordered by
// how many hotels they have.
func cityPrices(catalog []hotels.Hotel) (order []string, min map[string]float64) {
	min = map[string]float64{}
	count := map[string]int{}
	for _, h := range catalog {
		c := strings.TrimSpace(h.City)
		if c == "" || h.PricePerNight <= 0 {
			continue
		}
		if _, ok := min[c]; !ok || h.PricePerNight < min[c] {
			min[c] = h.PricePerNight
		}
		count[c]++
	}
	for c := range min {
		order = append(order, c)
	}
	sort.Slice(order, func(i, j int) bool {
		if count[order[i]] != count[order[j]] {
			return count[order[i]] > count[order[j]]
		}
		return order[i] < order[j]
	})
	return order, min
}

func cityPriceReply(city string, catalog []hotels.Hotel) string {
	_, min := cityPrices(catalog)
	for c, p := range min {
		if strings.EqualFold(c, city) {
			return fmt.Sprintf("Hotels in %s start from ₹%s per room per night, before GST. Room prices depend on your dates and room type; choose your dates to see live prices.", c, inr(p))
		}
	}
	return ""
}

func priceSummaryReply(catalog []hotels.Hotel) string {
	order, min := cityPrices(catalog)
	if len(order) == 0 {
		return "I couldn't load prices just now. Please try again in a moment, or browse hotels to see current prices."
	}
	if len(order) > 6 {
		order = order[:6]
	}
	var items, itemsHi []string
	for _, c := range order {
		items = append(items, fmt.Sprintf("%s from ₹%s", c, inr(min[c])))
		itemsHi = append(itemsHi, fmt.Sprintf("%s ₹%s se", c, inr(min[c])))
	}
	return fmt.Sprintf("Starting prices per room per night, before GST: %s. Room prices depend on your dates and room type, so pick a destination to see live prices.", strings.Join(items, ", "))
}
