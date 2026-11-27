package chatbot

// Cleaning model text before it leaves the API: internal blocks and control
// tags removed, other XML-like tags unwrapped (their visible text kept), raw
// JSON dropped, and anything that would still expose how the assistant works,
// or isn't English, replaced. The website repeats this as a second
// line of defence (features/chat/sanitize.ts).

import (
	"regexp"
	"strings"
)

var (
	// Go's regexp has no back-references, so each internal block is listed.
	internalBlocks = func() []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, tag := range []string{"think", "thinking", "reasoning", "tool_call", "tool_calls", "tool_response", "tool_result",
			"function_call", "function", "system", "instruction", "instructions", "internal", "scratchpad", "analysis"} {
			out = append(out, regexp.MustCompile(`(?is)<`+tag+`\b[^>]*>.*?(</`+tag+`\s*>|$)`))
		}
		return out
	}()
	controlTag  = regexp.MustCompile(`(?i)</?(meta|route|routing|intent|metadata|context|tool|tools|data)\b[^>]*/?>`)
	anyTag      = regexp.MustCompile(`(?i)</?[a-z][\w:-]*(\s[^<>]*)?/?>`)
	jsonFence   = regexp.MustCompile("(?s)```(?:json)?\\s*[\\[{].*?[\\]}]\\s*```")
	jsonObject  = regexp.MustCompile(`\{\s*"[\w$-]+"\s*:[^{}]*(\{[^{}]*\}[^{}]*)*\}`)
	jsonArray   = regexp.MustCompile(`(?s)\[\s*\{\s*"[\w$-]+"\s*:.*?\}\s*\]`)
	mdLink      = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdBold      = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	mdHeading   = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	trailingWS  = regexp.MustCompile(`[ \t]+\n`)
	manyNewline = regexp.MustCompile(`\n{3,}`)

	exposesWords = regexp.MustCompile(`(?i)\b(ollama|qwen|llm|large language model|language model|system prompt|prompt injection|api|apis|endpoint|database|sql|json|backend|back-end|frontend|front-end|tool call|tool calls|tool_call|search_hotels|get_hotel_details|check_room_availability|list_my_bookings|get_booking_status|hotel_id|room_type_id|rate_plan_id|booking_id|conversation_id|stack trace|null pointer|http \d{3}|status code)\b`)
)

// stripInternal removes markup a visitor must never see and keeps the rest.
func stripInternal(s string) string {
	for _, re := range internalBlocks {
		s = re.ReplaceAllString(s, "")
	}
	s = controlTag.ReplaceAllString(s, "")
	s = jsonFence.ReplaceAllString(s, "")
	s = jsonArray.ReplaceAllString(s, "")
	s = jsonObject.ReplaceAllString(s, "")
	s = anyTag.ReplaceAllString(s, "")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdBold.ReplaceAllString(s, "$1$2")
	s = mdHeading.ReplaceAllString(s, "")
	s = trailingWS.ReplaceAllString(s, "\n")
	s = manyNewline.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// exposesInternals reports text that names the assistant's machinery or isn't
// English (Devanagari, or Hindi written in Roman script).
func exposesInternals(s string) bool {
	return exposesWords.MatchString(s) || devanagari.MatchString(s) || looksHinglish(s)
}

var hinglishWords = regexp.MustCompile(`(?i)\b(aap|aapka|aapki|aapke|hai|hain|nahi|nahin|kijiye|kripya|mein|chahiye|abhi|sakta|sakte|karun|kaise|kitna|kitne|mila|mile|milega|wala|wali|liye|swagat|dhoondh\w*|batayiye|dekhiye|ek|aur|ya|yahan|wahan|kahan)\b`)

// looksHinglish reports Hindi written in Roman script: two or more distinct
// Hindi words, so a single word inside a hotel name doesn't count.
func looksHinglish(s string) bool {
	seen := map[string]bool{}
	for _, m := range hinglishWords.FindAllString(s, -1) {
		seen[strings.ToLower(m)] = true
	}
	return len(seen) >= 2
}

func safeFallback() string {
	return "I couldn't confirm that just now. Please try asking in a different way, or browse hotels on SKYMCM."
}

// repeatsEarlierReply reports a model reply that copies an earlier assistant
// turn of this conversation (a small model sometimes echoes its last answer
// instead of answering the new message).
func repeatsEarlierReply(reply string, turns []Turn) bool {
	r := strings.Join(strings.Fields(strings.ToLower(stripInternal(reply))), " ")
	if len(r) < 40 {
		return false
	}
	for _, t := range turns {
		if t.Role != "assistant" {
			continue
		}
		prev := strings.Join(strings.Fields(strings.ToLower(t.Text)), " ")
		if len(prev) >= 40 && (r == prev || strings.HasPrefix(r, prev[:min(len(prev), 120)]) || strings.HasPrefix(prev, r[:min(len(r), 120)])) {
			return true
		}
	}
	return false
}

// cleanReply is what the guest sees of a model reply: cleaned, and when that
// isn't safe, an answer written from the lookups' data (if any) or a plain
// fallback.
func cleanReply(reply string, run *toolRun) string {
	clean := stripInternal(reply)
	if clean != "" && !exposesInternals(clean) {
		return clean
	}
	if fb := fallbackReply(run); fb != "" {
		return fb
	}
	return safeFallback()
}
