package scrapemate

// SkipsProxy reports whether the fetch for job must ignore the mate's proxy.
// Google search jobs do not implement this. Email extraction does.
func SkipsProxy(job IJob) bool {
	bypass, ok := job.(interface{ DirectFetch() bool })

	return ok && bypass.DirectFetch()
}
