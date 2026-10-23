package httpapi

import (
	"context"
	"time"
)

// pollInterval is how often waitFor looks. The reads are of the informer cache, so this costs
// the API server nothing.
const pollInterval = 100 * time.Millisecond

// waitFor calls done at once and then every 100ms until it is true, the timeout passes (false,
// nil), done fails (that error), or ctx ends (its error). It is how a request waits for the
// operator before it answers 200 or 202.
func waitFor(ctx context.Context, timeout time.Duration, done func(context.Context) (bool, error)) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if ok, err := done(ctx); err != nil || ok {
			return ok, err
		}
		select {
		case <-tick.C:
		case <-timer.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}
