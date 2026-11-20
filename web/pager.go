package web

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ListPageSize is how many rows a paged list on the campaign page shows.
const ListPageSize = 10

// Job orders for the job lists: the chosen status comes first, the rest keep
// their order (newest first).
const (
	JobOrderNewest  = ""
	JobOrderOK      = "ok"
	JobOrderFailed  = "failed"
	JobOrderWorking = "working"
)

// ValidJobOrder reports whether order is one of the job list orders.
func ValidJobOrder(order string) bool {
	switch order {
	case JobOrderNewest, JobOrderOK, JobOrderFailed, JobOrderWorking:
		return true
	default:
		return false
	}
}

// jobOrderRank is the position of a status for an order; lower comes first.
func jobOrderRank(order, status string) int {
	switch order {
	case JobOrderOK, JobOrderFailed:
		if status == order {
			return 0
		}

		return 1
	case JobOrderWorking:
		switch status {
		case StatusWorking:
			return 0
		case StatusPending:
			return 1
		default:
			return 2
		}
	default:
		// Newest: running jobs first, as the job list always did.
		if status == StatusWorking {
			return 0
		}

		return 1
	}
}

// sortJobsBy orders jobs for order, keeping the current order within a status.
func sortJobsBy(jobs []Job, order string) {
	slices.SortStableFunc(jobs, func(a, b Job) int {
		return jobOrderRank(order, a.Status) - jobOrderRank(order, b.Status)
	})
}

// Pager is one page of a list on the campaign page, with links that keep the
// other lists' pages and the open tab.
type Pager struct {
	Page    int
	Pages   int
	Total   int
	From    int
	To      int
	HasPrev bool
	HasNext bool
	// Links are relative ("?a=1&b=2#tab") so they stay on the current page.
	FirstURL string
	PrevURL  string
	NextURL  string
	LastURL  string
}

// HasPages reports whether there is more than one page.
func (p Pager) HasPages() bool {
	return p.Pages > 1
}

// newPager pages total rows by size from the page number in query[param].
// It returns the pager and the [start, end) range of the rows to show.
func newPager(query url.Values, param string, total, size int, anchor string) (p Pager, start, end int) {
	page, _ := strconv.Atoi(query.Get(param))
	pages := max((total+size-1)/size, 1)
	page = min(max(page, 1), pages)

	start = (page - 1) * size
	end = min(start+size, total)

	link := func(n int) string {
		q := url.Values{}
		for k, v := range query {
			q[k] = slices.Clone(v)
		}

		if n <= 1 {
			q.Del(param)
		} else {
			q.Set(param, strconv.Itoa(n))
		}

		out := "?" + q.Encode()
		if anchor != "" {
			out += "#" + anchor
		}

		return out
	}

	p = Pager{
		Page:     page,
		Pages:    pages,
		Total:    total,
		From:     min(start+1, total),
		To:       end,
		HasPrev:  page > 1,
		HasNext:  page < pages,
		FirstURL: link(1),
		PrevURL:  link(page - 1),
		NextURL:  link(page + 1),
		LastURL:  link(pages),
	}

	return p, start, end
}

// pageQuery keeps only the list paging and order parameters of a request, so
// the live refresh of the campaign page shows the same pages.
func pageQuery(query url.Values, params ...string) string {
	q := url.Values{}

	for _, k := range params {
		if v := strings.TrimSpace(query.Get(k)); v != "" {
			q.Set(k, v)
		}
	}

	if len(q) == 0 {
		return ""
	}

	return "?" + q.Encode()
}
