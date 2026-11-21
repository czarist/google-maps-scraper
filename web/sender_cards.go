package web

import "strings"

// senderErrorLimit is how many failures a sender's error log shows.
const senderErrorLimit = 40

// SenderCard is one registered sender and its results since the sender address
// started being stored on each send.
type SenderCard struct {
	DialogID    string
	FromEmail   string
	FromName    string
	Host        string
	Port        int
	Username    string
	Encryption  string
	PasswordSet bool
	Sent        int
	Failed      int
	Errors      []MailLogEntry
	ErrorMore   int
}

// senderCards lists the campaign server first, then each extra server.
// A log row counts for every card with the same from address. Rows stored
// before the sender address was recorded are left out.
func senderCards(primary MailConfig, primaryPasswordSet bool, extras []SMTPAccount, log []MailLogEntry) []SenderCard {
	cards := make([]SenderCard, 0, 1+len(extras))

	p := primary.Normalize()
	cards = append(cards, SenderCard{
		DialogID:    "primary",
		FromEmail:   p.FromEmail,
		FromName:    p.FromName,
		Host:        p.Host,
		Port:        p.Port,
		Username:    p.Username,
		Encryption:  string(p.Encryption),
		PasswordSet: primaryPasswordSet,
	})

	for i := range extras {
		a := extras[i].Normalize()
		id := a.ID
		if id == "" {
			id = "extra"
		}

		cards = append(cards, SenderCard{
			DialogID:    id,
			FromEmail:   a.FromEmail,
			FromName:    a.FromName,
			Host:        a.Host,
			Port:        a.Port,
			Username:    a.Username,
			Encryption:  string(a.Encryption),
			PasswordSet: a.PasswordSet,
		})
	}

	for _, entry := range log {
		from := strings.ToLower(strings.TrimSpace(entry.FromEmail))
		if from == "" {
			continue
		}

		for i := range cards {
			if strings.ToLower(cards[i].FromEmail) != from {
				continue
			}

			switch entry.Status {
			case MailStatusSent:
				cards[i].Sent++
			case MailStatusFailed:
				cards[i].Failed++
				if len(cards[i].Errors) < senderErrorLimit {
					cards[i].Errors = append(cards[i].Errors, entry)
				} else {
					cards[i].ErrorMore++
				}
			}
		}
	}

	return cards
}
