package chatbot

// The assistant's domain: hotels and stays on SKYMCM. Messages outside it get
// a short reply before any lookup or model call, so an unrelated question
// never triggers a hotel search, an availability check or a booking lookup.
// Kept in step with the website's own check (features/chat/domain.ts).

import (
	"regexp"
	"strings"
)

const (
	offTopicReply = "I'm here to help you with hotels and travel bookings on SKYMCM. You can ask me about hotel availability, prices, amenities, bookings, cancellations or destinations. How can I assist you with your stay?"

	unsupportedReply = "Currently, I can assist you with hotel searches and bookings on SKYMCM. Please let me know if you need help finding a hotel."

	greetingReply = "Hello! Welcome to SKYMCM. I can help you find hotels, check room availability and prices, or answer questions about your booking. What would you like to do?"
)

var (
	domainNorm = regexp.MustCompile(`[^a-z0-9\x{00C0}-\x{024F}\x{0900}-\x{097F}]+`)

	greetingMsg = regexp.MustCompile(`^(hi+|hello+|hey+|hiya|namaste|namaskar|hola|good (morning|afternoon|evening|day))( (there|ji|team|skymcm|assistant|bot))?$`)

	thanksMsg = regexp.MustCompile(`^(thanks?( you)?( so much| a lot| very much)?|thank u|thx|ty|dhanyavaad|dhanyawad|shukriya)( ji)?$`)
	byeMsg    = regexp.MustCompile(`^(bye+|goodbye|good bye|see you|see ya|good night|tata|alvida)$`)
	ackMsg    = regexp.MustCompile(`^(ok+|okay|okk|k|cool|great|nice|awesome|perfect|fine|alright|all right|got it|sure|theek hai|thik hai|accha|achha)$`)

	abuseWords = regexp.MustCompile(`\b(chutiy\w*|madarchod|madar chod|mc|bc|bhenchod|behenchod|bhen chod|bsdk|bhosdi\w*|bhosad\w*|gandu|gaand\w*|lund|lavde|lawde|randi|harami|haramkhor|kamina|kamine|kameena|kameene|saala|saale|kutta|kutte|kutiya|pagal|paagal|bewakoof|bewkoof|bevakoof|bevkoof|ullu|gadha|gadhe|gadhi|nalayak|besharam|tatti|bakwas|chup kar|chal hat|abe saale|fuck\w*|shit|bitch\w*|bastard|asshole|dick|motherfucker|stupid|idiot|dumb ?ass|moron|shut up)\b`)

	domainHotelWords = regexp.MustCompile(`\b(hotels?|stays?|staying|rooms?|suites?|beds?|lodges?|inns?|palace|residency|guest ?houses?|homestays?|hostels?|dharamshala|accommodation|propert(y|ies)|check ?in|check ?out|checkin|checkout|amenit(y|ies)|facilit(y|ies)|wi ?fi|internet|parking|pool|gym|ac|air ?condition\w*|breakfast|meals?|lift|elevator|laundry|room service|geyser|hot water|towels?|extra bed|prices?|pricing|rates?|tariff|costs?|cheap|cheapest|budget|expensive|discounts?|offers?|coupons?|promo( code)?|gst|tax(es)?|fees?|available|availability|vacanc(y|ies)|book|booked|booking|bookings|reservations?|reserve|cancel\w*|refunds?|pay|paid|paying|payment|payments|upi|cards?|wallets?|invoice|receipt|confirm\w*|pending|status|policy|policies|pets?|child(ren)?|kids|couples?|unmarried|id proof|aadhaa?r|passport|documents?|night|nights|guests?|skymcm|maynstay|website|login|log in|sign in|account|support|contact|vendor|kamra|kamre|kiraya|kimat|keemat|sasta|mehenga|rukna|rehna)\b`)

	// Where a stay is: allowed for a hotel or a listed destination.
	stayLocation = regexp.MustCompile(`\b(near|nearby|nearest|distance|far|km|kilomet\w*|reach|directions?|route|located|location|address|map|walk|walking|station|railway|airport|bus stand|landmarks?|attractions?|ghats?|temples?)\b`)

	// "How far is the railway station?": a stay question; the assistant asks
	// which hotel when none is named.
	transitDistance = regexp.MustCompile(`\b(how far|distance|near|nearest|km)\b.*\b(railway|station|airport|bus stand)\b|\b(railway|station|airport|bus stand)\b.*\b(how far|distance|near|nearest|km)\b`)

	// General travel the assistant doesn't cover.
	generalTravel = regexp.MustCompile(`\b(weather|temperature|climate|rain|raining|monsoon|forecast|season|best time|sightseeing|itinerary|trip plan\w*|tourist places|places to visit|things to do|must see|holiday|vacation|honeymoon|packing|what to pack|visa)\b|\bplan\b.*\b(trip|tour|holiday|vacation)\b`)

	domainOffTopic = regexp.MustCompile(`\b(capital of|prime minister|president of|chief minister|who (is|was|are|won|invented|discovered|wrote)|what is (a |an )?(python|java|javascript|ai|love|life)|cricket|ipl|football|match score|movies?|films?|songs?|lyrics|poem|poetry|jokes?|riddle|recipe|coding|write (a |some )?code|program(ming|me)?|python|javascript|typescript|java|sql|html|css|react|software|computer|laptop|mobile phone|math\w*|calculate|equation|homework|assignment|essay|translate|stocks?|share market|sensex|bitcoin|crypto\w*|politic\w*|elections?|government|modi|rahul|bjp|congress|religion|girlfriend|boyfriend|marry|dating|meaning of life|horoscope|astrolog\w*|are you (a )?(human|robot|bot|real|married|single)|your (age|name|salary)|how old are you)\b|\d+\s*[-+*/x×÷^]\s*\d+`)

	// Services SKYMCM doesn't offer yet (car rental, tours and airport
	// transfer are "coming soon" on the home page).
	unsupportedWords = regexp.MustCompile(`\b(cars?|cabs?|taxis?|car rental|rent(al)? (a )?(car|bike|scooter)|bikes? on rent|self drive|chauffeur|flights?|air ?tickets?|plane tickets?|airlines?|train tickets?|rail tickets?|irctc|bus tickets?|volvo bus|airport (transfer|pickup|pick up|drop)|tour packages?|travel packages?|holiday packages?|tours?|tour guide)\b`)
	facilityAsk      = regexp.MustCompile(`\b(does|do|is there|are there|has|have|provide|provides|offer|offers)\b`)
	facilityPlace    = regexp.MustCompile(`\b(hotels?|property|resort|homestay|guest ?house)\b`)

	vendorWords = regexp.MustCompile(`\b(list|add|register|onboard|put)\b.*\b(my|our)\b.*\b(property|hotel|homestay|guest ?house|resort|villa)\b|\bbecome a (vendor|partner|host)\b|\bvendor (registration|signup|sign up|account)\b`)
	resortWords = regexp.MustCompile(`\b(resorts?|villas?)\b`)
	plainHotel  = regexp.MustCompile(`\bhotels?\b`)
	dineWords   = regexp.MustCompile(`\b(restaurants?|cafes?|dine|dining|eateries|shops?|shopping|markets?|street food)\b`)
	dineHotel   = regexp.MustCompile(`\b(hotels?|room service|breakfast)\b`)

	dateWords    = regexp.MustCompile(`\b(jan(uary)?|feb(ruary)?|mar(ch)?|apr(il)?|may|june?|july?|aug(ust)?|sep(t|tember)?|oct(ober)?|nov(ember)?|dec(ember)?|monday|tuesday|wednesday|thursday|friday|saturday|sunday|today|tonight|tomorrow|day after|next week|this week|kal|parso|adults?|people|persons?|days?|weeks?)\b`)
	refersBack   = regexp.MustCompile(`\b(it|its|this|that|these|those|they|them|their|there|here|one|ones|same|above|first|second|third|last)\b`)
	hinglishAsk  = regexp.MustCompile(`\b(chahiye|batao|bataiye|dikhao|dikhaiye|milega|milegi)\b`)
	latinWord    = regexp.MustCompile(`[a-z\x{00C0}-\x{024F}]{3,}`)
	devHotelText = regexp.MustCompile(`होटल|कमरा|कमरे|रूम|बुकिंग|किराया|ठहर|रुक|धर्मशाला|आगरा|वाराणसी|बनारस|लखनऊ|अयोध्या|वृंदावन|ऋषिकेश|हरिद्वार|चेक इन|रिफंड|कैंसल|भुगतान`)
)

// A request to reach the support team, in English or Hinglish: always support.
var supportAsk = regexp.MustCompile(`(?i)\b(customer (support|care|service)|support( team)?|help ?desk|helpline|contact (you|us|support|skymcm|number|no|details)|contact number|phone number|mobile number|call (you|us|support)|talk to (someone|a human|a person|an agent|support|customer)|speak (to|with) (someone|a human|a person|an agent|your team|support)|agent|human|email (address|id)|whatsapp|complaint|shikayat|sampark)\b|\bhow (can|do) i (contact|reach|call|email) (you|skymcm|support|the team)\b`)

// A general plea for help ("I need help", "madad chahiye", "help karo"):
// support, unless the same message asks for a stay ("I need help finding a
// hotel in Agra" is a hotel search).
var supportHelpAsk = regexp.MustCompile(`(?i)\b(need help|get help|want help|help (me|please|pls|chahiye|chaahiye|karo|kijiye|kariye|kar do|karna)|madad|sahayata|sahayta|sahaayata)\b|^\s*help\s*[.!?]*\s*$`)

// The same in Devanagari (word boundaries don't apply to this script).
var (
	supportAskDev  = regexp.MustCompile(`संपर्क|सम्पर्क|कस्टमर केयर|कस्टमर सपोर्ट|ग्राहक सेवा|हेल्पलाइन|शिकायत`)
	supportHelpDev = regexp.MustCompile(`मदद|सहायता|सहायत`)
	stayRequest    = regexp.MustCompile(`(?i)\b(hotels?|rooms?|stays?|resorts?|homestays?|accommodation|lodges?)\b|होटल|कमरा|कमरे|रूम`)
)

// isSupportRequest is a request to reach SKYMCM's customer support team, in
// English, Hinglish or Hindi. The reply is always English.
func isSupportRequest(msg string) bool {
	if supportAsk.MatchString(msg) || supportAskDev.MatchString(msg) {
		return true
	}
	if supportHelpAsk.MatchString(msg) || supportHelpDev.MatchString(msg) {
		return !stayRequest.MatchString(msg)
	}
	return false
}

// SetSiteURL sets the public website address used in support replies.
func (s *Service) SetSiteURL(u string) { s.siteURL = strings.TrimRight(strings.TrimSpace(u), "/") }

// domainText lower-cases s, keeps only letters and digits, and pads it so
// " agra " matches a whole word.
func domainText(s string) string {
	return " " + strings.TrimSpace(domainNorm.ReplaceAllString(strings.ToLower(s), " ")) + " "
}

// smallTalkReply answers greetings, thanks and goodbyes, redirects abuse,
// and turns down services SKYMCM doesn't offer. Empty text otherwise.
func smallTalkReply(msg string) (text, intent string) {
	t := strings.TrimSpace(domainText(msg))
	switch {
	case t == "":
		return "", ""
	case abuseWords.MatchString(" " + t + " "):
		return offTopicReply, IntentGeneral
	case greetingMsg.MatchString(t):
		return greetingReply, IntentGreeting
	case thanksMsg.MatchString(t):
		return "You're welcome! Is there anything else I can help you with for your stay?", IntentGeneral
	case byeMsg.MatchString(t):
		return "Goodbye, and have a wonderful trip! Come back any time you need help with hotels or bookings.", IntentGeneral
	case ackMsg.MatchString(t):
		return "Sure. Let me know if you need anything else for your stay.", IntentGeneral
	}
	// Another booking platform (Agoda, Booking.com...): SKYMCM-only help,
	// before anything else could read it as a SKYMCM booking question.
	if text := thirdPartyReply(msg); text != "" {
		return text, IntentThirdParty
	}
	if isUnsupportedService(msg) {
		return unsupportedReply, IntentGeneral
	}
	return serviceAnswer(msg), IntentGeneral
}

func isUnsupportedService(msg string) bool {
	t := domainText(msg)
	if !unsupportedWords.MatchString(t) {
		return false
	}
	return !(facilityPlace.MatchString(t) && facilityAsk.MatchString(t))
}

// serviceAnswer answers questions about SKYMCM's own services from the site
// itself; empty for anything else.
func serviceAnswer(msg string) string {
	t := domainText(msg)
	switch {
	case vendorWords.MatchString(t):
		return "You can list your property on SKYMCM from the Become a Vendor page. Tell us about your property there; our team reviews every application before it goes live."
	case resortWords.MatchString(t) && !plainHotel.MatchString(t):
		return "You can browse resorts and private villas on our Resorts page, with beachfront, hill-station and heritage properties across India."
	case dineWords.MatchString(t) && !dineHotel.MatchString(t):
		return "Our Shop & Dine page has curated restaurants, markets, cafes and local experiences near your stay."
	}
	return ""
}

// isOffTopic reports whether msg is outside SKYMCM's hotel service. places
// are listed city and hotel names; hasContext means a search or conversation
// is under way, so "is it near the station?" refers back to it.
func isOffTopic(msg string, places []string, hasContext bool) bool {
	t := domainText(msg)
	if strings.TrimSpace(t) == "" {
		return false
	}
	if devanagari.MatchString(msg) {
		return !devHotelText.MatchString(msg)
	}
	hotelish := domainHotelWords.MatchString(t)
	if domainOffTopic.MatchString(t) || generalTravel.MatchString(t) {
		return !hotelish
	}
	if hotelish || dateWords.MatchString(t) || transitDistance.MatchString(t) {
		return false
	}
	namesPlace := false
	for _, p := range places {
		if p = strings.TrimSpace(domainText(p)); len(p) >= 3 && strings.Contains(t, " "+p+" ") {
			namesPlace = true
			break
		}
	}
	if namesPlace && (stayLocation.MatchString(t) || hinglishAsk.MatchString(t) || len(strings.Fields(t)) <= 3) {
		return false
	}
	if hasContext && (stayLocation.MatchString(t) || refersBack.MatchString(t) || len(strings.Fields(t)) <= 4) {
		return false
	}
	return latinWord.MatchString(t)
}
