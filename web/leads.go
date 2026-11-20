package web

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
)

// Lead is one business of a campaign, merged across its jobs.
type Lead struct {
	Name          string
	Category      string
	Address       string
	Phone         string
	WebsitePhones []string
	WhatsApp      []string
	// ProbableWhatsApp lists the WhatsApp links plus every mobile number found,
	// since Brazilian mobiles almost always have WhatsApp.
	ProbableWhatsApp []string
	Emails           []string
	Website          string
	Link             string
	Job              string
}

// LeadCSVHeader is the header of the campaign leads export.
var LeadCSVHeader = []string{
	"nome", "categoria", "endereco", "telefone", "telefones_site", "whatsapp",
	"provavel_whatsapp", "emails", "site", "link_maps", "tarefa",
}

// Row is the lead as a CSV row in LeadCSVHeader order.
func (l *Lead) Row() []string {
	return []string{
		l.Name, l.Category, l.Address, l.Phone, strings.Join(l.WebsitePhones, ", "),
		strings.Join(l.WhatsApp, ", "), strings.Join(l.ProbableWhatsApp, ", "),
		strings.Join(l.Emails, ", "), l.Website, l.Link, l.Job,
	}
}

// CampaignLeads reads every job CSV of a campaign and returns one lead per
// business: the same place found by two jobs (neighbor cities) is merged.
func (s *Service) CampaignLeads(ctx context.Context, campaignID string) ([]Lead, error) {
	jobs, err := s.CampaignJobList(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	byKey := make(map[string]*Lead)

	var order []string

	for i := range jobs {
		path, err := s.csvPath(jobs[i].ID)
		if err != nil {
			continue
		}

		if err := readLeads(path, jobs[i].Name, func(key string, l *Lead) {
			if prev, ok := byKey[key]; ok {
				mergeLead(prev, l)

				return
			}

			byKey[key] = l
			order = append(order, key)
		}); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	out := make([]Lead, 0, len(order))

	for _, k := range order {
		l := byKey[k]
		l.ProbableWhatsApp = probableWhatsApp(l)
		out = append(out, *l)
	}

	sort.SliceStable(out, func(a, b int) bool { return out[a].Name < out[b].Name })

	return out, nil
}

// readLeads calls add for each place of a job CSV, keyed by place id (or link).
func readLeads(path, jobName string, add func(key string, l *Lead)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}

	if err != nil {
		return err
	}

	col := make(map[string]int, len(header))
	for i, h := range header {
		col[h] = i
	}

	get := func(row []string, name string) string {
		if i, ok := col[name]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}

		return ""
	}

	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		l := &Lead{
			Name:          get(row, "title"),
			Category:      get(row, "category"),
			Address:       get(row, "address"),
			Phone:         get(row, "phone"),
			WebsitePhones: splitList(get(row, "website_phones")),
			WhatsApp:      splitList(get(row, "whatsapp")),
			Emails:        splitList(get(row, "emails")),
			Website:       get(row, "website"),
			Link:          get(row, "link"),
			Job:           jobName,
		}

		key := get(row, "place_id")
		if key == "" {
			key = l.Link
		}

		if key == "" {
			key = strings.ToLower(l.Name + "|" + l.Address)
		}

		add(key, l)
	}
}

func splitList(v string) []string {
	var out []string

	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// mergeLead adds what b knows that a does not.
func mergeLead(a, b *Lead) {
	if a.Phone == "" {
		a.Phone = b.Phone
	}

	if a.Website == "" {
		a.Website = b.Website
	}

	a.Emails = unionLower(a.Emails, b.Emails)
	a.WebsitePhones = union(a.WebsitePhones, b.WebsitePhones)
	a.WhatsApp = union(a.WhatsApp, b.WhatsApp)
}

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}

	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			a = append(a, v)
		}
	}

	return a
}

func unionLower(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[strings.ToLower(v)] = true
	}

	for _, v := range b {
		if !seen[strings.ToLower(v)] {
			seen[strings.ToLower(v)] = true
			a = append(a, v)
		}
	}

	return a
}

// probableWhatsApp returns the WhatsApp links plus the mobile numbers among the
// lead's phones, without repeats (compared by digits).
func probableWhatsApp(l *Lead) []string {
	seen := map[string]bool{}

	var out []string

	add := func(p string) {
		d := brDigits(p)
		if d == "" || seen[d] {
			return
		}

		seen[d] = true
		out = append(out, p)
	}

	for _, p := range l.WhatsApp {
		add(p)
	}

	for _, p := range append([]string{l.Phone}, l.WebsitePhones...) {
		if isBRMobile(p) {
			add(p)
		}
	}

	return out
}

// brDigits returns DDD + number (10 or 11 digits), dropping a +55 prefix.
func brDigits(p string) string {
	var b strings.Builder

	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}

	d := b.String()
	if (len(d) == 12 || len(d) == 13) && strings.HasPrefix(d, "55") {
		d = d[2:]
	}

	if len(d) != 10 && len(d) != 11 {
		return ""
	}

	return d
}

// isBRMobile reports whether p is a Brazilian mobile: DDD + 9 digits starting with 9.
func isBRMobile(p string) bool {
	d := brDigits(p)

	return len(d) == 11 && d[2] == '9'
}
