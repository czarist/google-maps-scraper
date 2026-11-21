package web

import "testing"

func TestMailAccountAtRotatesAndKeepsTheMessage(t *testing.T) {
	primary := MailConfig{
		Host: "one.example", Port: 587, Username: "one", Password: "secret-1",
		FromEmail: "one@example.com", FromName: "One",
		Subject: "Hello {{.Title}}", Body: "Same body", Encryption: MailStartTLS,
	}
	extras := []SMTPAccount{{
		Host: "two.example", Port: 465, Username: "two", Password: "secret-2",
		FromEmail: "two@example.com", Encryption: MailSSL,
	}}

	first := mailAccountAt(primary, extras, 0)
	if first.Host != "one.example" || first.Subject != primary.Subject || first.Body != primary.Body {
		t.Fatalf("first %+v", first)
	}

	second := mailAccountAt(primary, extras, 1)
	if second.Host != "two.example" || second.FromEmail != "two@example.com" || second.Password != "secret-2" {
		t.Fatalf("second %+v", second)
	}

	if second.Subject != primary.Subject || second.Body != primary.Body || second.FromName != "" {
		t.Fatalf("sender took the other sender's name: %+v", second)
	}

	named := mailAccountAt(primary, []SMTPAccount{{
		Host: "two.example", FromEmail: "two@example.com", FromName: "Two", Password: "secret-2",
	}}, 1)
	if named.FromName != "Two" || named.Subject != primary.Subject {
		t.Fatalf("own name %+v", named)
	}

	third := mailAccountAt(primary, extras, 2)
	if third.Host != "one.example" || third.FromEmail != "one@example.com" {
		t.Fatalf("third %+v", third)
	}

	only := mailAccountAt(primary, nil, 5)
	if only.Host != primary.Host {
		t.Fatalf("without extras %+v", only)
	}
}

func TestSMTPAccountValidate(t *testing.T) {
	ok := SMTPAccount{
		Host: "smtp.example.com", Password: "secret", FromEmail: "from@example.com",
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []SMTPAccount{
		{Password: "secret", FromEmail: "from@example.com"},
		{Host: "smtp.example.com", FromEmail: "from@example.com"},
		{Host: "smtp.example.com", Password: "secret"},
		{Host: "bad host", Password: "secret", FromEmail: "from@example.com"},
		{Host: "smtp.example.com", Password: "secret", FromEmail: "not-an-email"},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
