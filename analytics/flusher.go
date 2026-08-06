package analytics

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

var (
	// errRejected: the store refused the batch permanently (4xx). Dropped, not retried.
	errRejected = errors.New("analytics: batch permanently rejected")
	// errNotConfigured: the target env var vanished between New and delivery.
	errNotConfigured = errors.New("analytics: transport not configured")
	// errStopping: Shutdown fired while we were waiting between retries. The batch is
	// RETAINED and re-attempted by the drain under its bounded deadline.
	errStopping = errors.New("analytics: shutdown preempted retry")
)

// run is the flusher. It owns the batch buffer, so nothing else ever touches it.
func (c *Client) run() {
	defer close(c.stopped)
	t := time.NewTicker(c.flushIvl)
	defer t.Stop()

	batch := make([]Event, 0, c.batchSize)
	for {
		select {
		case e := <-c.q:
			batch = append(batch, e)
			if len(batch) >= c.batchSize {
				batch = c.flush(context.Background(), batch, false)
			}
		case <-t.C:
			batch = c.flush(context.Background(), batch, false)
		case <-c.stop:
			batch = c.drain(batch)
			return
		}
	}
}

// drain is the shutdown path: pull everything already queued, then deliver under the
// bounded context Shutdown supplied. The gRPC server has already stopped accepting by the
// time Descriptor.Shutdown runs, so nothing is enqueueing behind us and this terminates.
func (c *Client) drain(batch []Event) []Event {
	ctx := c.drainContext()
	for {
		select {
		case e := <-c.q:
			batch = append(batch, e)
			if len(batch) >= c.batchSize {
				batch = c.flush(ctx, batch, true)
			}
			continue
		default:
		}
		break
	}
	return c.flush(ctx, batch, true)
}

func (c *Client) drainContext() context.Context {
	if v, ok := c.drainCx.Load().(context.Context); ok && v != nil {
		return v
	}
	return context.Background()
}

// flush delivers one batch and returns the buffer to reuse.
//
// The batch is RETAINED (returned unchanged) only when a retry was preempted by
// shutdown, so the drain can re-attempt it. Every other outcome empties the buffer:
// silently holding a batch forever would wedge the flusher and turn a transient fault
// into total loss.
func (c *Client) flush(ctx context.Context, batch []Event, draining bool) []Event {
	if len(batch) == 0 {
		return batch
	}
	c.inFlight.Store(int64(len(batch)))
	err := c.deliver(ctx, batch, draining)
	c.inFlight.Store(0)

	switch {
	case err == nil, errors.Is(err, errRejected), errors.Is(err, errNotConfigured):
		// Already counted by deliver.
	case errors.Is(err, errStopping):
		return batch // keep it; drain() will try once more under its deadline
	default:
		// Deadline or cancellation, i.e. the drain ran out of time. Unrecoverable — the
		// process is exiting — but it must be COUNTED rather than vanish, because the
		// count is the only remaining evidence these events existed.
		c.dropped.Add(uint64(len(batch)))
		c.cfg.Log("error", fmt.Sprintf("analytics: %d event(s) undelivered: %v", len(batch), err))
	}
	return batch[:0]
}

// deliver sends one batch, retrying until it succeeds, is permanently rejected, ctx ends,
// or (outside the drain) shutdown preempts it.
//
// The status branching is the important part:
//
//	2xx        → done
//	4xx        → PERMANENT. Drop, loudly. A malformed event or an unknown key cannot be
//	             fixed by sending it again; retrying forever wedges the flusher, fills the
//	             queue behind it, and converts one bad event into total data loss.
//	5xx / err  → transient. Keep the batch and retry with capped, jittered backoff.
//
// Retries resend the IDENTICAL event ids, which is what makes the store's
// ReplacingMergeTree dedup correct rather than a double-count.
func (c *Client) deliver(ctx context.Context, batch []Event, draining bool) error {
	body, err := c.encodeBatch(batch)
	if err != nil {
		// Encoding will not start succeeding on a retry.
		c.rejected.Add(uint64(len(batch)))
		c.cfg.Log("error", fmt.Sprintf("analytics: could not encode %d event(s): %v", len(batch), err))
		return errRejected
	}

	// Clamped, not just capped later: an operator who lowers the cap below the initial
	// delay would otherwise get the 500ms default forever and no cap at all.
	delay := 500 * time.Millisecond
	if delay > c.maxBackff {
		delay = c.maxBackff
	}
	for {
		status, out, configured, terr := c.cfg.Transport(ctx, c.envKey, "POST", eventsPath, body)
		switch {
		case !configured:
			c.cfg.Log("warn", "analytics: "+c.envKey+" is unset — discarding batch")
			return errNotConfigured
		case terr == nil && status >= 200 && status < 300:
			c.sent.Add(uint64(len(batch)))
			return nil
		case terr == nil && status >= 400 && status < 500:
			c.rejected.Add(uint64(len(batch)))
			c.cfg.Log("error", fmt.Sprintf(
				"analytics: %d event(s) rejected with %d — dropping (a retry cannot fix this): %s",
				len(batch), status, truncate(out, 512)))
			return errRejected
		}

		c.retries.Add(1)
		// Full jitter on a capped exponential: without it every emitter in the fleet
		// retries in lockstep and hammers a store that is already struggling.
		timer := time.NewTimer(time.Duration(rand.Int63n(int64(delay) + 1)))

		// Outside the drain the wait must also watch c.stop. Without that, a permanently
		// failing store keeps this loop spinning on a Background context forever: the stop
		// is never observed, drain() never runs, and a SIGTERM during an outage delivers
		// NOTHING — precisely the loss the drain exists to prevent. During the drain the
		// channel is already closed, so watching it would abort the final attempt instantly.
		var stop <-chan struct{}
		if !draining {
			stop = c.stop
		}

		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-stop:
			timer.Stop()
			return errStopping
		}

		if delay < c.maxBackff {
			delay *= 2
			if delay > c.maxBackff {
				delay = c.maxBackff
			}
		}
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
