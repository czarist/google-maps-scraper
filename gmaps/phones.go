package gmaps

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

// maxWebsitePhones caps how many phone numbers are kept from one website.
const maxWebsitePhones = 5

var (
	// phonePrefixRe matches a phone number glued to the front of an email's
	// local part, e.g. "99545-3421contato@..." when a site prints
	// "WhatsApp: (51) 99545-3421contato@..." with no space in between.
	phonePrefixRe = regexp.MustCompile(`^[+(]?\d[\d\s().+-]*\d{4}[\s.-]?\d{4}`)

	// textPhoneRe finds Brazilian numbers written with an area code (DDD):
	// "(51) 99545-3421", "+55 51 3013-4545", "+55 (51) 3013 4545".
	// A bare "3013-4545" has no DDD, and "51 3013-4545" without parentheses or
	// +55 looks too much like other numbers, so both are left out on purpose.
	textPhoneRe = regexp.MustCompile(`(?:\+\s?55[\s.-]?\(?(\d{2})\)?|\((\d{2})\))[\s.-]?(9?\d{4})[\s.-]?(\d{4})`)

	nonDigitRe = regexp.MustCompile(`\D`)
)

// StripPhonePrefix removes a phone number glued to the start of an email's
// local part. The rest must start with a letter, so addresses that really
// begin with digits ("2020turma@...") are kept.
func StripPhonePrefix(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return email
	}

	local := email[:at]

	loc := phonePrefixRe.FindStringIndex(local)
	if loc == nil {
		return email
	}

	if digits := nonDigitRe.ReplaceAllString(local[:loc[1]], ""); len(digits) < 8 {
		return email
	}

	rest := local[loc[1]:]
	if rest == "" || !unicode.IsLetter(rune(rest[0])) {
		return email
	}

	return rest + email[at:]
}

// formatBRPhone validates a Brazilian number with its area code and formats it
// as "(51) 99545-3421". It returns false for anything that is not a real
// landline (8 digits starting 2-5) or mobile (9 digits starting with 9).
func formatBRPhone(ddd, number string) (string, bool) {
	if len(ddd) != 2 || ddd[0] == '0' || ddd[1] == '0' {
		return "", false
	}

	switch {
	case len(number) == 9 && number[0] == '9':
		return fmt.Sprintf("(%s) %s-%s", ddd, number[:5], number[5:]), true
	case len(number) == 8 && number[0] >= '2' && number[0] <= '5':
		return fmt.Sprintf("(%s) %s-%s", ddd, number[:4], number[4:]), true
	}

	return "", false
}

// phoneFromDigits reads a full number such as "5551995453421" or
// "51995453421" (from tel: or WhatsApp links).
func phoneFromDigits(digits string) (string, bool) {
	if strings.HasPrefix(digits, "55") && (len(digits) == 12 || len(digits) == 13) {
		digits = digits[2:]
	}

	digits = strings.TrimPrefix(digits, "0")

	if len(digits) != 10 && len(digits) != 11 {
		return "", false
	}

	return formatBRPhone(digits[:2], digits[2:])
}

// websitePhones collects phone numbers with area code from a website: tel: and
// WhatsApp links first, then numbers written in the page text.
func websitePhones(doc *goquery.Document) []string {
	seen := map[string]bool{}

	var phones []string

	add := func(p string, ok bool) {
		if ok && !seen[p] && len(phones) < maxWebsitePhones {
			seen[p] = true
			phones = append(phones, p)
		}
	}

	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		lower := strings.ToLower(href)

		switch {
		case strings.HasPrefix(lower, "tel:"):
			add(phoneFromDigits(nonDigitRe.ReplaceAllString(href[len("tel:"):], "")))
		case strings.Contains(lower, "wa.me/"):
			path := href[strings.Index(lower, "wa.me/")+len("wa.me/"):]
			path = strings.SplitN(path, "?", 2)[0]
			add(phoneFromDigits(nonDigitRe.ReplaceAllString(path, "")))
		case strings.Contains(lower, "whatsapp.com/send"):
			if i := strings.Index(lower, "phone="); i >= 0 {
				v := strings.SplitN(href[i+len("phone="):], "&", 2)[0]
				add(phoneFromDigits(nonDigitRe.ReplaceAllString(v, "")))
			}
		}
	})

	doc.Find("script, style, noscript").Remove()

	text := doc.Text()

	for _, m := range textPhoneRe.FindAllStringSubmatchIndex(text, -1) {
		// The number must not run on into more digits ("(51) 3013-45456").
		if m[1] < len(text) && text[m[1]] >= '0' && text[m[1]] <= '9' {
			continue
		}

		ddd := submatch(text, m, 1)
		if ddd == "" {
			ddd = submatch(text, m, 2)
		}

		add(formatBRPhone(ddd, submatch(text, m, 3)+submatch(text, m, 4)))
	}

	return phones
}

func submatch(s string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}

	return s[m[2*i]:m[2*i+1]]
}
