package web

import (
	"strconv"
	"testing"
)

func TestRepeatCitiesCounts(t *testing.T) {
	caps, err := RepeatCities(RepeatCapitals, "")
	if err != nil || len(caps) != 27 {
		t.Fatalf("capitals: %d, %v", len(caps), err)
	}

	all, err := RepeatCities(RepeatMunicipalities, "")
	if err != nil || len(all) < 5570 {
		t.Fatalf("municipalities: %d, %v", len(all), err)
	}

	rs, err := RepeatCities(RepeatMunicipalities, "rs")
	if err != nil || len(rs) < 490 {
		t.Fatalf("RS municipalities: %d, %v", len(rs), err)
	}

	for _, c := range rs {
		if c.UF != "RS" {
			t.Fatalf("got %+v in RS list", c)
		}
	}

	if _, err := RepeatCities(RepeatMunicipalities, "XX"); err == nil {
		t.Fatal("unknown state should fail")
	}

	total := 0
	for _, u := range MunicipalityUFs() {
		total += u.Count
	}

	if total != len(all) || len(MunicipalityUFs()) != 27 {
		t.Fatalf("UF counts: %d states, %d total", len(MunicipalityUFs()), total)
	}
}

func TestExpandJobByCities(t *testing.T) {
	tmpl := Job{ID: "orig", Name: "Cosméticos", Data: JobData{
		Keywords: []string{"cosméticos, porto alegre", "  perfumaria  "},
		Proxies:  []string{"http://p:1"},
	}}

	n := 0
	next := func() string { n++; return "id" + strconv.Itoa(n) }

	jobs, err := ExpandJobByCities(&tmpl, []City{{"Porto Alegre", "RS"}, {"Curitiba", "PR"}}, next)
	if err != nil {
		t.Fatal(err)
	}

	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}

	j := jobs[1]
	if j.ID != "id2" || j.Name != "Cosméticos · Curitiba/PR" {
		t.Fatalf("got %q %q", j.ID, j.Name)
	}

	want := []string{"cosméticos Curitiba PR", "perfumaria Curitiba PR"}
	for i := range want {
		if j.Data.Keywords[i] != want[i] {
			t.Fatalf("keyword %d: got %q, want %q", i, j.Data.Keywords[i], want[i])
		}
	}

	if len(j.Data.Proxies) != 1 || tmpl.Data.Keywords[0] != "cosméticos, porto alegre" {
		t.Fatal("template job changed or proxies lost")
	}

	if _, err := ExpandJobByCities(&Job{Data: JobData{Keywords: []string{", curitiba"}}}, brCapitals, next); err == nil {
		t.Fatal("a query with no term should fail")
	}
}
