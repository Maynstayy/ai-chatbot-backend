package chatbot

// The assistant always replies in English, whatever language the guest
// writes in (English, Hindi, Hinglish or anything else). Guests' Hindi and
// Hinglish are still understood; only the replies are English.

import "regexp"

// devanagari matches Hindi script. It must never appear in a reply.
var devanagari = regexp.MustCompile(`[\x{0900}-\x{097F}]`)
