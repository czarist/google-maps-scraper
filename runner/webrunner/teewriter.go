package webrunner

import (
	"context"

	"github.com/gosom/scrapemate"
	"golang.org/x/sync/errgroup"
)

// teeWriter sends every result to all of its writers.
//
// scrapemate runs each configured writer on the same results channel, so two
// writers would split the results between them instead of both getting all
// of them. Wrapping them in a single teeWriter avoids that.
type teeWriter struct {
	writers []scrapemate.ResultWriter
}

func newTeeWriter(writers ...scrapemate.ResultWriter) scrapemate.ResultWriter {
	if len(writers) == 1 {
		return writers[0]
	}

	return &teeWriter{writers: writers}
}

// Run forwards results until the input channel is closed, like a single
// writer would. It does not stop on context cancellation: the job context is
// canceled when scraping ends, and the last results must still reach every
// writer.
func (t *teeWriter) Run(ctx context.Context, in <-chan scrapemate.Result) error {
	var g errgroup.Group

	outs := make([]chan scrapemate.Result, len(t.writers))

	for i, w := range t.writers {
		ch := make(chan scrapemate.Result)
		outs[i] = ch

		g.Go(func() error {
			err := w.Run(ctx, ch)

			// Keep draining, so a writer that stops early never blocks the others.
			for range ch {
			}

			return err
		})
	}

	for result := range in {
		for _, ch := range outs {
			ch <- result
		}
	}

	for _, ch := range outs {
		close(ch)
	}

	return g.Wait()
}
