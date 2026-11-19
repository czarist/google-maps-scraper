package web

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// cities_br.txt lists Brazil's municipalities (IBGE) as "UF;Name" lines.
//
//go:embed cities_br.txt
var citiesBRRaw string

// Modes for repeating a job's searches in other cities.
const (
	RepeatNone           = ""
	RepeatCapitals       = "capitals"
	RepeatMunicipalities = "municipalities"
)

// City is a Brazilian city and its state code.
type City struct {
	Name string
	UF   string
}

// Label is how the city is shown in job names, e.g. "Curitiba/PR".
func (c City) Label() string {
	return c.Name + "/" + c.UF
}

// brCapitals are the 26 state capitals plus Brasília (DF).
var brCapitals = []City{
	{"Rio Branco", "AC"}, {"Maceió", "AL"}, {"Macapá", "AP"}, {"Manaus", "AM"},
	{"Salvador", "BA"}, {"Fortaleza", "CE"}, {"Brasília", "DF"}, {"Vitória", "ES"},
	{"Goiânia", "GO"}, {"São Luís", "MA"}, {"Cuiabá", "MT"}, {"Campo Grande", "MS"},
	{"Belo Horizonte", "MG"}, {"Belém", "PA"}, {"João Pessoa", "PB"}, {"Curitiba", "PR"},
	{"Recife", "PE"}, {"Teresina", "PI"}, {"Rio de Janeiro", "RJ"}, {"Natal", "RN"},
	{"Porto Alegre", "RS"}, {"Porto Velho", "RO"}, {"Boa Vista", "RR"}, {"Florianópolis", "SC"},
	{"São Paulo", "SP"}, {"Aracaju", "SE"}, {"Palmas", "TO"},
}

// UFCount is a state and how many municipalities it has.
type UFCount struct {
	UF    string
	Count int
}

var (
	municipalitiesOnce sync.Once
	municipalities     []City
	municipalityUFs    []UFCount
)

func loadMunicipalities() {
	municipalitiesOnce.Do(func() {
		for _, line := range strings.Split(citiesBRRaw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			uf, name, ok := strings.Cut(line, ";")
			if !ok {
				continue
			}

			municipalities = append(municipalities, City{Name: name, UF: uf})

			if n := len(municipalityUFs); n > 0 && municipalityUFs[n-1].UF == uf {
				municipalityUFs[n-1].Count++
			} else {
				municipalityUFs = append(municipalityUFs, UFCount{UF: uf, Count: 1})
			}
		}
	})
}

// MunicipalityUFs lists the states with their municipality counts, for the form.
func MunicipalityUFs() []UFCount {
	loadMunicipalities()

	return municipalityUFs
}

// MunicipalityCount is the number of municipalities in Brazil.
func MunicipalityCount() int {
	loadMunicipalities()

	return len(municipalities)
}

// CapitalCount is the number of state capitals plus Brasília.
func CapitalCount() int {
	return len(brCapitals)
}

// RepeatCities returns the cities a job is repeated in.
// uf limits municipalities to one state; empty means the whole country.
func RepeatCities(mode, uf string) ([]City, error) {
	switch mode {
	case RepeatNone:
		return nil, nil
	case RepeatCapitals:
		return brCapitals, nil
	case RepeatMunicipalities:
		loadMunicipalities()

		uf = strings.ToUpper(strings.TrimSpace(uf))
		if uf == "" {
			return municipalities, nil
		}

		var out []City

		for _, c := range municipalities {
			if c.UF == uf {
				out = append(out, c)
			}
		}

		if len(out) == 0 {
			return nil, fmt.Errorf("unknown state %q", uf)
		}

		return out, nil
	default:
		return nil, fmt.Errorf("unknown repeat mode %q", mode)
	}
}

// searchTerm drops the place from a query written as "term, place".
func searchTerm(query string) string {
	term, _, _ := strings.Cut(query, ",")

	return strings.TrimSpace(term)
}

// cityKeywords turns each query into "term City UF", e.g. "cosméticos Curitiba PR".
func cityKeywords(queries []string, c City) []string {
	out := make([]string, 0, len(queries))

	for _, q := range queries {
		if term := searchTerm(q); term != "" {
			out = append(out, term+" "+c.Name+" "+c.UF)
		}
	}

	return out
}

// errNoSearchTerm means every query was empty once the place was removed.
var errNoSearchTerm = errors.New("missing keywords")

// ExpandJobByCities makes one job per city from a template job.
// Each copy gets a new id, the city in its name, and its queries rewritten for the city.
func ExpandJobByCities(tmpl *Job, cities []City, newID func() string) ([]Job, error) {
	jobs := make([]Job, 0, len(cities))

	for _, c := range cities {
		kw := cityKeywords(tmpl.Data.Keywords, c)
		if len(kw) == 0 {
			return nil, errNoSearchTerm
		}

		j := *tmpl
		j.ID = newID()
		j.Name = strings.TrimSpace(tmpl.Name + " · " + c.Label())
		j.Data.Keywords = kw

		jobs = append(jobs, j)
	}

	return jobs, nil
}
