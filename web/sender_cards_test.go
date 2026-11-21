package web

import "testing"

func TestPort465IsStoredAsSSL(t *testing.T) {
	mail := (&MailConfig{Host: "smtps.example", Port: 465, Encryption: MailStartTLS}).Normalize()
	if mail.Encryption != MailSSL {
		t.Fatalf("mail encryption %s", mail.Encryption)
	}

	account := (&SMTPAccount{Host: "smtps.example", Port: 465, Encryption: MailStartTLS, Password: "secret", FromEmail: "a@example.com"}).Normalize()
	if account.Encryption != MailSSL {
		t.Fatalf("account encryption %s", account.Encryption)
	}

	if !smtpImplicitTLS(&MailConfig{Port: 465, Encryption: MailStartTLS}) {
		t.Fatal("port 465 STARTTLS was left on a plain connection")
	}
}

func TestSenderCardsCountEachFromAddress(t *testing.T) {
	primary := MailConfig{Host: "smtp.zoho.com", Port: 465, Encryption: MailSSL, FromEmail: "one@example.com", FromName: "One"}
	extras := []SMTPAccount{{
		ID: "extra-1", Host: "smtps.example", Port: 465, Encryption: MailSSL, FromEmail: "two@example.com",
	}}
	log := []MailLogEntry{
		{Email: "a@x.com", Status: MailStatusSent, FromEmail: "one@example.com"},
		{Email: "b@x.com", Status: MailStatusFailed, FromEmail: "two@example.com", Error: "timeout"},
		{Email: "c@x.com", Status: MailStatusSent, FromEmail: ""},
	}

	cards := senderCards(primary, true, extras, log)
	if len(cards) != 2 {
		t.Fatalf("cards %d", len(cards))
	}

	if cards[0].DialogID != "primary" || cards[0].Sent != 1 || cards[0].Failed != 0 {
		t.Fatalf("primary %+v", cards[0])
	}

	if cards[1].DialogID != "extra-1" || cards[1].Sent != 0 || cards[1].Failed != 1 || cards[1].Errors[0].Email != "b@x.com" {
		t.Fatalf("extra %+v", cards[1])
	}
}
