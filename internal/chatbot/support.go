package chatbot

// Customer support: SKYMCM's verified contact channels, returned both as the
// reply text (for plain clients) and as structured, clickable channels the
// website renders as buttons. Only configured, valid details are shown; a
// channel that isn't set (WhatsApp, until an official number is published)
// is left out rather than shown as a placeholder.

import (
	"regexp"
	"strings"
)

// Published on the website's Contact page and footer.
const (
	DefaultSupportPhone = "+91 7607956595"
	DefaultSupportEmail = "skymcm17@gmail.com"

	supportPhoneHours = "Mon–Sat, 9am–9pm IST"
	supportEmailReply = "We reply within 24 hours"
)

// SupportInfo is the configured contact details. Empty fields fall back to
// the published defaults, except WhatsApp, which stays hidden when empty.
type SupportInfo struct {
	Phone    string
	Email    string
	WhatsApp string
}

// SupportChannel is one clickable way to reach the support team.
type SupportChannel struct {
	Kind  string `json:"kind"` // whatsapp, phone, email or contact
	Label string `json:"label"`
	Href  string `json:"href"`
	Value string `json:"value,omitempty"`
	Note  string `json:"note,omitempty"`
}

var (
	supportEmailPattern = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)
	nonDigit            = regexp.MustCompile(`\D`)
)

// supportDigits returns the phone number's digits when it is a plausible
// international number (10 to 15 digits), otherwise "".
func supportDigits(phone string) string {
	d := nonDigit.ReplaceAllString(phone, "")
	if len(d) < 10 || len(d) > 15 {
		return ""
	}
	return d
}

// SetSupport sets the contact details used in support replies. Invalid
// values are dropped: a published default is used for phone and email, and
// WhatsApp is hidden.
func (s *Service) SetSupport(info SupportInfo) {
	info.Phone, info.Email, info.WhatsApp = strings.TrimSpace(info.Phone), strings.TrimSpace(info.Email), strings.TrimSpace(info.WhatsApp)
	if supportDigits(info.Phone) == "" {
		info.Phone = DefaultSupportPhone
	}
	if !supportEmailPattern.MatchString(info.Email) {
		info.Email = DefaultSupportEmail
	}
	if supportDigits(info.WhatsApp) == "" {
		info.WhatsApp = ""
	}
	s.support = info
}

func (s *Service) supportInfo() SupportInfo {
	info := s.support
	if info.Phone == "" {
		info.Phone = DefaultSupportPhone
	}
	if info.Email == "" {
		info.Email = DefaultSupportEmail
	}
	return info
}

func (s *Service) contactURL() string {
	site := s.siteURL
	if site == "" {
		site = "https://skymcm.com"
	}
	return site + "/contact"
}

// supportChannels lists the verified channels, WhatsApp first when set.
func (s *Service) supportChannels() []SupportChannel {
	info := s.supportInfo()
	var out []SupportChannel
	if d := supportDigits(info.WhatsApp); d != "" {
		out = append(out, SupportChannel{Kind: "whatsapp", Label: "WhatsApp Support", Href: "https://wa.me/" + d, Value: info.WhatsApp})
	}
	out = append(out,
		SupportChannel{Kind: "phone", Label: "Call Us", Href: "tel:+" + supportDigits(info.Phone), Value: info.Phone, Note: supportPhoneHours},
		SupportChannel{Kind: "email", Label: "Email Support", Href: "mailto:" + info.Email, Value: info.Email, Note: supportEmailReply},
		SupportChannel{Kind: "contact", Label: "Contact Us", Href: s.contactURL()},
	)
	return out
}

// supportReply is the support answer as plain text, listing the same
// channels as supportChannels.
func (s *Service) supportReply() string {
	info := s.supportInfo()
	var b strings.Builder
	b.WriteString("Sure! Our customer support team is here to help you.\n\nYou can contact us through the following options:\n")
	if info.WhatsApp != "" {
		b.WriteString("• WhatsApp: " + info.WhatsApp + "\n")
	}
	b.WriteString("• Call us: " + info.Phone + " (" + supportPhoneHours + ")\n")
	b.WriteString("• Email us: " + info.Email + " (we reply within 24 hours)\n")
	b.WriteString("• Contact Us page: " + s.contactURL() + "\n\n")
	b.WriteString("Please choose your preferred way to connect with our team.")
	return b.String()
}
