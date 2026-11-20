//nolint:testpackage // countCSV is unexported.
package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCountCSVCountsPhonesAndWhatsApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.csv")
	csv := "title,phone,emails,website_phones,whatsapp\n" +
		"A,(51) 3013-4545,a@x.com,,\n" +
		"B,,,(51) 99545-3421,(51) 99545-3421\n" +
		"C,,,,\n"

	if err := os.WriteFile(path, []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := countCSV(path)
	if err != nil {
		t.Fatal(err)
	}

	if st.places != 3 || st.withEmail != 1 || st.withPhone != 2 || st.withWhatsApp != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestJobTablePutsRunningJobsFirst(t *testing.T) {
	jobs := paginatedJobs(3)
	jobs[2].Status = StatusWorking // the oldest job is the one running

	srv := newTestServerWithRepo(t, &mockJobRepo{jobs: jobs})

	page, err := srv.svc.ListJobTable(t.Context(), "", "", 1)
	if err != nil {
		t.Fatal(err)
	}

	if page.Jobs[0].Status != StatusWorking || page.Jobs[0].ID != jobs[2].ID {
		t.Fatalf("first row = %+v, want the running job", page.Jobs[0])
	}
}
