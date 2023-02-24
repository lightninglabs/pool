package pool

import "time"

const (
	// maxBackOff is the maximum amount of that we'll wait to retry to
	// w/e operation we're wanting to back off.
	maxBackOff = time.Minute

	// minBackOff is the smallest amount of time we'll wait until we retry
	// the given operation.
	minBackOff = time.Millisecond * 100
)

// backOffer implements a simple exponential back off scheme using a channel
// and a timer.
type backOffer struct {
	backOffPeriod time.Duration
}

// backOff returns a channel that should be used to wait for the current back
// off period before doubling the period for the subsequent call to this
// method.
func (b *backOffer) backOff(logID string) <-chan time.Time {
	// The first time around, this'll multiply by zero meaning there's no
	// back off, which'll trigger the base case below to use the min back
	// off.
	b.backOffPeriod *= 2

	if b.backOffPeriod == 0 {
		b.backOffPeriod = minBackOff
	}

	if b.backOffPeriod > maxBackOff {
		b.backOffPeriod = maxBackOff
	}

	log.Debugf("Backing off for period of time %v for %v", b.backOffPeriod,
		logID)

	return time.After(b.backOffPeriod)
}
