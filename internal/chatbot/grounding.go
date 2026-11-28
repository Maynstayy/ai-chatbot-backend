package chatbot

// The SKYMCM website's listings are the only source of truth for hotels,
// prices, ratings and availability. This file holds the deterministic
// answers that keep the model from filling gaps with invented hotels:
//
//   - a place the guest names that SKYMCM doesn't list ("hotels in Mumbai")
//     gets a plain "no hotels listed" reply, even when the website sent the
//     previous search's hotels as context;
//   - "hotels that give a full refund on cancellation" is a search over the
//     listings' free-cancellation data, not the refund-timing answer;
//   - "under 1000" is filtered strictly on the listed starting prices, and a
//     bare budget follow-up keeps the current city;
//   - a model reply that quotes prices, ratings, review counts or hotel
//     names the listings don't contain is replaced.
//
// Listed cities, states and hotel names always come from the live catalogue
// (s.catalog); no city list is built in.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
)

// --- places the guest names ---------------------------------------------------

var (
	// "hotels in Mumbai", "stay at Goa for 2", "near Juhu beach".
	placeAfter = regexp.MustCompile(`(?i)\b(?:in|at|near|around)\s+([a-z][a-z.' -]{1,40}?)(?:\s+(?:for|under|below|with|from|on|between|this|next|that|which|who|where|offering|providing|having|giving|and|to|during|within|starting|priced|costing|please|tonight|today|tomorrow)\b|\s+\d|[,.!?;:]|$)`)
	// "Mumbai hotels", "show me Goa stays".
	placeBeforeHotels = regexp.MustCompile(`(?i)^\s*(?:(?:show|find|list|get|any|search)\s+(?:me\s+)?)?(?:some\s+|the\s+)?([a-z][a-z.' -]{2,30}?)\s+(?:hotels?|stays?|homestays?|resorts?|guest ?houses?|lodges?)\b`)
)

// Words that are never a place on their own: references back, time and
// budget words, generic places and search verbs. A candidate made only of
// these (or starting with a reference) is not treated as a place.
var nonPlaceWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`this that these those there here it its the a an my your our their his her same above below
		first second third last next previous other another such hotel hotels room rooms stay stays property properties area areas
		city town centre center central locality neighbourhood neighborhood place places region side part
		budget price prices range rate rates cost cheap cheaper cheapest affordable low lowest high luxury luxurious premium
		good best better nice top popular new all any some more less few many every show find list get search me us
		january february march april may june july august september october november december jan feb mar apr jun jul aug sep sept oct nov dec
		today tomorrow tonight weekend weekends week weeks month months year years summer winter monsoon season festival holidays holiday
		night nights day days morning evening advance free cancellation cancel refund refunds refundable full fully booking bookings total
		station railway airport bus stand temple temples ghat ghats market markets river beach beaches mall hospital university college fort lake
		walking distance walk drive minutes km kilometre kilometers metres meters reach range of with family couple couples friends group
		which what who whom whose where when why how is are was were be been am do does did can could would should will shall may might must
		have has had i im you he she we they them him tell about something anything want wants need needs like looking look book give
		suggest recommend help please hi hello hey ok okay yes no not and or but for to on by as if so just also very really much see check
		compare one ones kind type sort options option`) {
		nonPlaceWords[w] = true
	}
}

// placeCandidates are the phrases the message names as where to stay,
// trimmed of non-place words at both ends ("best Mumbai" -> "Mumbai").
func placeCandidates(msg string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{placeAfter, placeBeforeHotels} {
		maxWords := 4
		if re == placeBeforeHotels {
			maxWords = 2
		}
		for _, m := range re.FindAllStringSubmatch(msg, -1) {
			ws := strings.Fields(strings.Trim(m[1], ".'- "))
			// "New" stays when it starts a longer name ("New Delhi").
			for len(ws) > 0 && nonPlaceWords[strings.ToLower(ws[0])] && !(strings.EqualFold(ws[0], "new") && len(ws) > 1) {
				ws = ws[1:]
			}
			for len(ws) > 0 && nonPlaceWords[strings.ToLower(ws[len(ws)-1])] {
				ws = ws[:len(ws)-1]
			}
			if len(ws) > 0 && len(ws) <= maxWords {
				out = append(out, strings.Join(ws, " "))
			}
		}
	}
	return out
}

// isPlaceLike reports whether a candidate could be a place name: not a
// reference ("this hotel"), not only generic words ("my budget",
// "december"), and no digits.
func isPlaceLike(c string) bool {
	ws := strings.Fields(strings.ToLower(c))
	if len(ws) == 0 || strings.ContainsAny(c, "0123456789") {
		return false
	}
	switch ws[0] {
	case "this", "that", "these", "those", "there", "here", "it", "its", "my", "your", "our", "same":
		return false
	}
	for _, w := range ws {
		if !nonPlaceWords[w] {
			return true
		}
	}
	return false
}

// listedPlace reports whether the phrase names something in the live
// catalogue: a city, a state or country, a hotel or a hotel's location.
func listedPlace(c string, catalog []hotels.Hotel) bool {
	p := strings.TrimSpace(words(c))
	if strings.HasPrefix(p, "the ") {
		p = strings.TrimSpace(strings.TrimPrefix(p, "the "))
	}
	if len(p) < 3 {
		return true // too short to call unlisted
	}
	match := func(field string) bool {
		f := strings.TrimSpace(words(field))
		return f != "" && (f == p || strings.Contains(" "+f+" ", " "+p+" ") || strings.Contains(" "+p+" ", " "+f+" "))
	}
	for _, h := range catalog {
		if match(h.City) || match(h.State) || match(h.Country) || match(h.Name) || match(h.Location) {
			return true
		}
	}
	return false
}

// unlistedPlaceIn returns the place the message asks about when SKYMCM lists
// nothing there ("Mumbai"), or "" when every place it names is listed, it
// names none, or the catalogue couldn't be loaded.
func unlistedPlaceIn(msg string, catalog []hotels.Hotel) string {
	if len(catalog) == 0 {
		return ""
	}
	var unlisted string
	for _, c := range placeCandidates(msg) {
		if !isPlaceLike(c) {
			continue
		}
		if listedPlace(c, catalog) {
			return ""
		}
		if unlisted == "" {
			unlisted = c
		}
	}
	return titleWords(unlisted)
}

func titleWords(s string) string {
	ws := strings.Fields(s)
	for i, w := range ws {
		r := []rune(strings.ToLower(w))
		if len(r) > 0 {
			r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		}
		ws[i] = string(r)
	}
	return strings.Join(ws, " ")
}

// listedCities is "Varanasi, Agra and Ayodhya" from the live catalogue.
func listedCities(catalog []hotels.Hotel) string {
	order, _ := cityPrices(catalog)
	switch len(order) {
	case 0:
		return ""
	case 1:
		return order[0]
	}
	return strings.Join(order[:len(order)-1], ", ") + " and " + order[len(order)-1]
}

func noHotelsListedReply(place string, catalog []hotels.Hotel) string {
	text := fmt.Sprintf("No hotels are currently listed in %s on SKYMCM.", place)
	if cities := listedCities(catalog); cities != "" {
		text += " We currently have hotels in " + cities + "."
	}
	return text
}

// listedCity is the catalogue's spelling of a city name, or "".
func listedCity(name string, catalog []hotels.Hotel) string {
	n := strings.TrimSpace(name)
	if n == "" {
		return ""
	}
	for _, h := range catalog {
		if c := strings.TrimSpace(h.City); strings.EqualFold(c, n) {
			return c
		}
	}
	return ""
}

func inCity(h hotels.Hotel, city string) bool {
	return city == "" || strings.EqualFold(strings.TrimSpace(h.City), city)
}

// --- budgets --------------------------------------------------------------------

var (
	strictBudgetWord = regexp.MustCompile(`(?i)^\s*(?:under|below|less than|cheaper than)\b`)
	// A follow-up that is only a budget: "under 1000", "and below 2k?",
	// "max ₹1500 per night".
	budgetOnly = regexp.MustCompile(`(?i)^\s*(?:(?:and|but|ok|okay|now|only|just|show|me|ones?|those|hotels?|stays?|options?|something|anything|please|what about|how about)\s+)*(?:under|below|upto|up to|less than|cheaper than|within|max(?:imum)?|budget(?: of| is)?)\s*(?:₹|rs\.?|inr)?\s*\d[\d,]*\s*(?:k\b)?\s*(?:rs|rupees|inr|/-)?\s*(?:per night|a night|only|please)?\s*[.!?]*\s*$`)
)

// budgetLimit is the ceiling the message states and whether it is strict:
// "under / below / less than 1000" excludes 1000, "up to / within / max"
// includes it.
func budgetLimit(msg string) (limit float64, strict bool) {
	m := budgetCeiling.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	limit = budgetFromText(msg)
	return limit, limit > 0 && strictBudgetWord.MatchString(m[0])
}

func withinBudget(price, limit float64, strict bool) bool {
	if limit <= 0 {
		return true
	}
	if strict {
		return price < limit
	}
	return price <= limit
}

func budgetLabel(limit float64, strict bool) string {
	if strict {
		return "under ₹" + inr(limit)
	}
	return "up to ₹" + inr(limit)
}

// budgetReply lists a city's listed hotels within the stated budget,
// straight from the catalogue's starting prices: nothing is estimated.
func (s *Service) budgetReply(ctx context.Context, run *toolRun, city, msg string) *directReply {
	limit, strict := budgetLimit(msg)
	active := true
	// The website filters max_price inclusively; "under" then drops the
	// limit itself here.
	list, err := s.hotels.GetHotels(ctx, hotels.HotelFilter{City: city, MaxPrice: limit, IsActive: &active})
	if err != nil {
		return &directReply{text: fmt.Sprintf("I couldn't load hotels in %s just now. Please try again in a moment.", city), intent: IntentHotelSearch}
	}
	var match []hotels.Hotel
	for _, h := range list {
		if h.IsActive && inCity(h, city) && h.PricePerNight > 0 && withinBudget(h.PricePerNight, limit, strict) {
			match = append(match, h)
		}
	}
	label := budgetLabel(limit, strict)
	if len(match) == 0 {
		text := fmt.Sprintf("No hotels in %s are listed %s right now.", city, label)
		if _, low := cityPrices(s.catalog(ctx)); low[city] > 0 {
			text += fmt.Sprintf(" The lowest starting price there is ₹%s a night, before GST.", inr(low[city]))
		}
		return &directReply{text: text, intent: IntentHotelSearch}
	}
	sort.SliceStable(match, func(i, j int) bool { return match[i].PricePerNight < match[j].PricePerNight })
	shown := match
	if len(shown) > maxHotelResults {
		shown = shown[:maxHotelResults]
	}
	for _, h := range shown {
		run.hotels = append(run.hotels, toCard(h))
	}
	return &directReply{
		text:   fmt.Sprintf("Sure! Here are some hotels available in %s %s.", city, label),
		intent: IntentHotelSearch,
		search: &SearchSummary{Destination: city, MaxPrice: limit, TotalMatches: len(match)},
	}
}

// --- refundable-hotel searches ---------------------------------------------------

var (
	refundEligibility = regexp.MustCompile(`(?i)\b(?:full(?:y)?\s+refunds?|full(?:y)?\s+refundable|100\s*%\s*refunds?|complete\s+refunds?|refundable|free[\s-]*cancel\w*|cancel\w*\s+(?:for\s+)?free|no\s+cancellation\s+(?:fees?|charges?|penalty))\b|\brefunds?\s+(?:on|for|if|when|after)\s+(?:the\s+)?(?:booking\s+)?cancel\w*`)
	refundTiming      = regexp.MustCompile(`(?i)\b(?:how\s+long|how\s+many\s+days|when\s+(?:will|do|does|can|would|should)|kab|kitne\s+din|refund\s+(?:status|time|timeline|processing)|processing\s+time|not\s+(?:yet\s+)?(?:received|credited)|credited|my\s+refund|get\s+my\s+money)\b`)
	stayWords         = regexp.MustCompile(`(?i)\b(?:hotels?|stays?|homestays?|resorts?|guest ?houses?|lodges?|properties|accommodations?|places\s+to\s+stay)\b`)
)

// isRefundableSearch is a request to find hotels whose bookings can be
// cancelled for a refund - not a question about when a refund arrives.
func isRefundableSearch(msg string) bool {
	return refundEligibility.MatchString(msg) && stayWords.MatchString(msg) && !refundTiming.MatchString(msg)
}

const maxRefundCards = 5

// refundableReply lists hotels the listings mark as free cancellation, in
// the city when one is known. The listings carry a free-cancellation flag
// and, for some hotels, the cancellation window in hours; they don't state a
// refund percentage, so the reply never promises a full refund.
func (s *Service) refundableReply(ctx context.Context, run *toolRun, city, msg string) *directReply {
	catalog := s.catalog(ctx)
	if len(catalog) == 0 {
		return &directReply{text: "I couldn't load SKYMCM's listings just now, so I can't check which hotels offer free cancellation. Please try again in a moment.", intent: IntentHotelSearch}
	}
	limit, strict := budgetLimit(msg)
	var listed, free []hotels.Hotel
	for _, h := range catalog {
		if !inCity(h, city) || !withinBudget(h.PricePerNight, limit, strict) {
			continue
		}
		listed = append(listed, h)
		if h.FreeCancellation {
			free = append(free, h)
		}
	}
	where := "on SKYMCM"
	if city != "" {
		where = "in " + city
	}
	if limit > 0 {
		where += " " + budgetLabel(limit, strict)
	}
	terms := "SKYMCM's listings mark these hotels as free cancellation but don't state a refund percentage, so a full refund isn't guaranteed. Each hotel's exact cancellation and refund terms are shown on the booking page before you pay."
	if len(free) == 0 {
		text := fmt.Sprintf("None of the %d hotels listed %s are marked free cancellation right now.", len(listed), where)
		switch len(listed) {
		case 0:
			text = fmt.Sprintf("No hotels are listed %s right now.", where)
		case 1:
			text = fmt.Sprintf("The only hotel listed %s isn't marked free cancellation right now.", where)
		}
		return &directReply{
			text:   text + " Each hotel's cancellation terms are shown on its booking page before you pay.",
			intent: IntentHotelSearch,
		}
	}
	sort.SliceStable(free, func(i, j int) bool {
		ri, rj := free[i].TotalReviews > 0, free[j].TotalReviews > 0
		if ri != rj {
			return ri
		}
		if free[i].Rating != free[j].Rating {
			return free[i].Rating > free[j].Rating
		}
		return free[i].PricePerNight < free[j].PricePerNight
	})
	shown := free
	if len(shown) > maxRefundCards {
		shown = shown[:maxRefundCards]
	}
	for _, h := range shown {
		run.hotels = append(run.hotels, toCard(h))
	}
	var b strings.Builder
	if city != "" {
		fmt.Fprintf(&b, "%d of the %d hotels listed %s are marked free cancellation.", len(free), len(listed), where)
	} else {
		counts := map[string]int{}
		var order []string
		for _, h := range free {
			c := strings.TrimSpace(h.City)
			if counts[c] == 0 {
				order = append(order, c)
			}
			counts[c]++
		}
		sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
		parts := make([]string, len(order))
		for i, c := range order {
			parts[i] = fmt.Sprintf("%s (%d)", c, counts[c])
		}
		fmt.Fprintf(&b, "%d hotels listed %s are marked free cancellation: %s.", len(free), where, strings.Join(parts, ", "))
	}
	if len(free) > len(shown) {
		fmt.Fprintf(&b, " Here are %d of them.", len(shown))
	}
	var windows []string
	for _, h := range shown {
		if h.CancellationHours != nil && *h.CancellationHours > 0 && len(windows) < 3 {
			windows = append(windows, fmt.Sprintf("%s (up to %d hours before check-in)", h.Name, *h.CancellationHours))
		}
	}
	if len(windows) > 0 {
		b.WriteString(" Cancellation windows the hotels list: " + strings.Join(windows, "; ") + ".")
	}
	b.WriteString(" " + terms)
	if city == "" {
		b.WriteString(" Tell me a city to see only its hotels.")
	}
	d := &directReply{text: b.String(), intent: IntentHotelSearch}
	if city != "" {
		d.search = &SearchSummary{Destination: city, TotalMatches: len(free)}
		if limit > 0 {
			d.search.MaxPrice = limit
		}
	}
	return d
}

// --- checking the model's reply against the listings ------------------------------

var (
	replyMoney   = regexp.MustCompile(`(?i)(?:₹|\brs\.?|\binr)\s*([\d,]+(?:\.\d+)?)|([\d,]+(?:\.\d+)?)\s*(?:rupees|\binr\b|/-)`)
	replyRating  = regexp.MustCompile(`(?i)\b(\d(?:\.\d)?)\s*(?:/\s*5|out of 5|stars?|-star)\b|\brat(?:ed|ing)(?:\s+of)?\s*(?:is\s*)?(\d(?:\.\d)?)\b`)
	replyReviews = regexp.MustCompile(`(?i)\b(\d[\d,]*)\s+(?:guest\s+)?reviews?\b`)
	replyHotel   = regexp.MustCompile(`\b(?:The\s+)?(?:[A-Z][\w'&.-]*\s+){0,3}(?:Hotel|Hotels|Resort|Inn|Palace|Residency|Lodge|Suites|Homestay|Guest\s?House|Villa)\b(?:\s+[A-Z][\w'&.-]*){0,3}`)
)

func parseNumber(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return v, err == nil
}

// knownFigures are the prices, taxes, ratings and review counts the reply
// may quote: those in this turn's lookups and in the live catalogue, plus
// any number the guest wrote.
func (s *Service) knownFigures(ctx context.Context, run *toolRun, in Input) []float64 {
	var known []float64
	addHotel := func(price, tax, rating float64, reviews int) {
		known = append(known, price, tax, rating, float64(reviews))
	}
	for _, h := range s.catalog(ctx) {
		addHotel(h.PricePerNight, h.TaxPerNight, h.Rating, h.TotalReviews)
	}
	for _, h := range run.hotels {
		addHotel(h.PricePerNight, h.TaxPerNight, h.Rating, h.TotalReviews)
	}
	for _, r := range run.roomTypes {
		known = append(known, r.PricePerNight, r.TaxPerNight, r.RatePlanPricePerNight, r.RatePlanTaxPerNight, r.ComboPricePerNight,
			float64(r.AvailableCount), float64(r.Capacity), float64(r.RoomsNeeded))
		if r.RatePlanStayTotal != nil {
			known = append(known, *r.RatePlanStayTotal)
		}
	}
	for _, m := range regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`).FindAllString(in.Message+" "+in.Context.describe(), -1) {
		if v, ok := parseNumber(m); ok {
			known = append(known, v)
		}
	}
	return known
}

func isKnown(v float64, known []float64) bool {
	for _, k := range known {
		if math.Abs(k-v) < 0.01 || (k > 0 && math.Abs(math.Round(k)-v) < 0.01) {
			return true
		}
	}
	return false
}

// unsupportedClaim reports whether a model reply states a price, rating,
// review count or hotel name the listings don't contain.
func (s *Service) unsupportedClaim(ctx context.Context, reply string, run *toolRun, in Input) bool {
	known := s.knownFigures(ctx, run, in)
	for _, re := range []*regexp.Regexp{replyMoney, replyRating, replyReviews} {
		for _, m := range re.FindAllStringSubmatch(reply, -1) {
			for _, g := range m[1:] {
				if g == "" {
					continue
				}
				if v, ok := parseNumber(g); ok && !isKnown(v, known) {
					return true
				}
			}
		}
	}
	catalog := s.catalog(ctx)
	for _, name := range replyHotel.FindAllString(reply, -1) {
		n := strings.TrimSpace(words(name))
		n = strings.TrimSpace(strings.TrimPrefix(n, "the "))
		if n == "" || n == "hotel" || n == "hotels" {
			continue
		}
		found := false
		for _, h := range catalog {
			hn := strings.TrimSpace(words(h.Name))
			if strings.Contains(" "+hn+" ", " "+n+" ") || strings.Contains(" "+n+" ", " "+hn+" ") {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return contradictsListing(reply, catalog)
}

var (
	replySentence = regexp.MustCompile(`[^.!?\n]+[.!?]?`)
	// "free cancellation", and the same said as not offered.
	freeCancelClaim  = regexp.MustCompile(`(?i)free[\s-]*cancel\w*|fully\s+refundable|full\s+refund`)
	noFreeCancelWord = regexp.MustCompile(`(?i)\b(?:not|no|isn't|is not|doesn't|does not|don't|without)\b[^.!?]{0,25}(?:free[\s-]*cancel|refundable|full\s+refund)|\bnon[\s-]*refundable`)
	// Listed prices are before GST, so a price "including GST" is wrong.
	priceWithTax = regexp.MustCompile(`(?i)\b(?:includ\w*|inclusive of|with)\s+(?:all\s+)?(?:gst|taxes|tax)\b`)
)

// contradictsListing reports a reply that states a listed hotel's
// free-cancellation status opposite to its listing ("Old Yogi Lodge... It
// is not free cancellation" for a free-cancellation hotel), or calls listed
// prices tax-inclusive. "It" and "this hotel" refer to the last hotel named.
func contradictsListing(reply string, catalog []hotels.Hotel) bool {
	if priceWithTax.MatchString(reply) && replyMoney.MatchString(reply) {
		return true
	}
	var last *hotels.Hotel
	for _, sentence := range replySentence.FindAllString(reply, -1) {
		w := " " + strings.TrimSpace(words(sentence)) + " "
		for i := range catalog {
			if name := strings.TrimSpace(words(catalog[i].Name)); len(name) >= 5 && strings.Contains(w, " "+name+" ") {
				last = &catalog[i]
			}
		}
		if last == nil || !freeCancelClaim.MatchString(sentence) {
			continue
		}
		if noFreeCancelWord.MatchString(sentence) == last.FreeCancellation {
			return true
		}
	}
	return false
}

// groundedReplacement is what is said instead of a reply with unsupported
// claims: built only from the listings and this turn's lookups.
func (s *Service) groundedReplacement(ctx context.Context, run *toolRun) string {
	catalog := s.catalog(ctx)
	for _, u := range run.uses {
		if u.Name != toolSearchHotels || u.Error != "" || u.Results != 0 {
			continue
		}
		var args struct {
			City string `json:"city"`
		}
		_ = json.Unmarshal(u.Args, &args)
		if c := strings.TrimSpace(args.City); c != "" {
			if listedCity(c, catalog) == "" {
				return noHotelsListedReply(titleWords(c), catalog)
			}
			return fmt.Sprintf("I couldn't find hotels matching that in %s on SKYMCM. Try a different budget or search the city's hotels.", listedCity(c, catalog))
		}
	}
	if len(run.hotels) > 0 {
		return "Here's what SKYMCM lists for that. The hotel cards show the live prices, ratings and policies from the website."
	}
	text := "I can only share hotels, prices and ratings that are listed on SKYMCM, and I couldn't confirm that from the listings."
	if cities := listedCities(catalog); cities != "" {
		text += " We currently have hotels in " + cities + ". Tell me a city and I'll show what's listed."
	}
	return text
}
