package chatbot

// Questions about other hotel booking platforms (Agoda, Booking.com,
// MakeMyTrip...). The assistant only helps with SKYMCM: it never explains
// another platform's booking steps, never claims a connection to one, and
// never claims to see or manage a reservation made there. Answered before
// any lookup or model call, in English. Kept in step with the website's own
// check (features/chat/domain.ts).

import "regexp"

// IntentThirdParty is reported for a question about another booking platform.
const IntentThirdParty = "third_party_platform"

// thirdPartyPlatforms maps how guests write a platform (on domainText: lower
// case, punctuation as spaces, so "Booking.com" is "booking com") to its name.
// Words that are also ordinary Hindi or English ("yatra", "trip") only count
// in their ".com" form.
var thirdPartyPlatforms = []struct {
	match *regexp.Regexp
	name  string
}{
	{regexp.MustCompile(`\bagoda\b`), "Agoda"},
	{regexp.MustCompile(`\bbooking ?com\b`), "Booking.com"},
	{regexp.MustCompile(`\b(makemytrip|make ?my ?trip|mmt)\b`), "MakeMyTrip"},
	{regexp.MustCompile(`\bgo ?ibibo\b`), "Goibibo"},
	{regexp.MustCompile(`\bexpedia\b`), "Expedia"},
	{regexp.MustCompile(`\bair ?bnb\b`), "Airbnb"},
	{regexp.MustCompile(`\boyo( rooms)?\b`), "OYO"},
	{regexp.MustCompile(`\btrivago\b`), "trivago"},
	{regexp.MustCompile(`\bclear ?trip\b`), "Cleartrip"},
	{regexp.MustCompile(`\bease ?my ?trip\b`), "EaseMyTrip"},
	{regexp.MustCompile(`\byatra com\b`), "Yatra"},
	{regexp.MustCompile(`\bhotels com\b`), "Hotels.com"},
	{regexp.MustCompile(`\btrip com\b`), "Trip.com"},
	{regexp.MustCompile(`\btrip ?advisor\b`), "Tripadvisor"},
	{regexp.MustCompile(`\bixigo\b`), "ixigo"},
	{regexp.MustCompile(`\btreebo\b`), "Treebo"},
	{regexp.MustCompile(`\bfab ?hotels\b`), "FabHotels"},
	{regexp.MustCompile(`\bhostelworld\b`), "Hostelworld"},
	{regexp.MustCompile(`\bkayak\b`), "Kayak"},
	{regexp.MustCompile(`\bpriceline\b`), "Priceline"},
}

var (
	// Another booking website or app, unnamed.
	otherPlatform = regexp.MustCompile(`\b(third ?party|partner sites?|(other|another|external|different) (hotel )?(booking )?(sites?|websites?|apps?|platforms?|portals?)|travel (agent|agency))\b`)

	// Managing a reservation already made there: cancel, refund, pay, change.
	thirdPartyManage = regexp.MustCompile(`\b(cancel\w*|refund\w*|money back|modif\w*|reschedul\w*|amend\w*|postpone\w*|charged|deducted|paid|pay|paying|payments?|invoice|receipt|confirmation|voucher|status|booked|reserved|my (booking|bookings|reservation|reservations|stay|order)|made a (booking|reservation)|book kiya|booking ki|kiya tha|change (my|the) (booking|reservation|dates?))\b`)

	// Whether SKYMCM is connected to, or compares with, that platform.
	thirdPartyRelation = regexp.MustCompile(`\b(partner\w*|connected|connect|integrat\w*|affiliat\w*|tie ?up|tied up|work with|works with|working with|same (as|company)|owned|part of|listed on|available on|cheaper|price match|compare|comparison|better than|vs|versus)\b`)
)

// thirdPartyName is the platform the message names ("" for an unnamed other
// site), and whether it mentions one at all.
func thirdPartyName(msg string) (string, bool) {
	t := domainText(msg)
	for _, p := range thirdPartyPlatforms {
		if p.match.MatchString(t) {
			return p.name, true
		}
	}
	return "", otherPlatform.MatchString(t)
}

// thirdPartyBookingReply answers how to search or book on another platform.
const thirdPartyBookingReply = "I'm here to assist you with hotel searches and bookings on SKYMCM.\n\n" +
	"I can help you explore hotels, compare available options, check room details and guide you through the booking process on our platform.\n\n" +
	"Would you like me to help you find a hotel?"

// thirdPartyReply is the answer to a message about another booking
// platform, or "" when it names none.
func thirdPartyReply(msg string) string {
	name, ok := thirdPartyName(msg)
	if !ok {
		return ""
	}
	t := domainText(msg)
	switch {
	case thirdPartyManage.MatchString(t):
		where, contact := "another platform", "the platform where it was booked"
		if name != "" {
			where, contact = name, name
		}
		return "I can only help with bookings made on SKYMCM, so I can't view or manage reservations made through " + where + ".\n\n" +
			"For changes, cancellations, payments or refunds on that reservation, please contact " + contact + " directly, as the booking was made there.\n\n" +
			"If you'd like, I can help you find a hotel or assist with a booking on SKYMCM."
	case thirdPartyRelation.MatchString(t):
		including := ""
		if name != "" {
			including = ", including " + name
		}
		return "SKYMCM is an independent hotel booking platform. We are not connected to other booking sites" + including +
			", and every booking is made directly on SKYMCM.\n\nWould you like me to help you find a hotel?"
	}
	return thirdPartyBookingReply
}
