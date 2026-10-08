package schema

import (
	"regexp"

	"github.com/nyaruka/phonenumbers"
)

// DefaultPhoneCountry is where a phone field starts when the form doesn't set one.
const DefaultPhoneCountry = "CL"

// phoneChars keeps the parser from pulling a number out of surrounding text ("tel: +56…").
var phoneChars = regexp.MustCompile(`^\+?[\d\s().-]+$`)

// PhoneCountry reports whether a phone field can start in this country: a known ISO code
// with its own calling code (Antarctica, for one, has none).
func PhoneCountry(code string) bool {
	return ValidCountry(code) && phonenumbers.GetCountryCodeForRegion(code) != 0
}

// ValidPhone reports whether s is a real phone number. The portal stores E.164
// ("+56961234567"); answers typed before the country picker existed ("+56 9 6123 4567",
// or a local "9 6123 4567" read in the field's country) still pass, so old submissions
// can be resubmitted. Mirrors validPhone() in web/src/lib/form-schema.ts.
func ValidPhone(s, country string) bool {
	if country == "" {
		country = DefaultPhoneCountry
	}
	if !phoneChars.MatchString(s) {
		return false
	}
	n, err := phonenumbers.Parse(s, country)
	return err == nil && phonenumbers.IsValidNumber(n)
}
