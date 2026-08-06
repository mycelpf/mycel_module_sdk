package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"
)

var (
	// ErrNotConfigured: no kpi module is wired. Render the screen without the number.
	ErrNotConfigured = errors.New("analytics: no kpi module configured")
	// ErrNotFound: the key is not a registered KPI. A caller bug, not a runtime fault.
	ErrNotFound = errors.New("analytics: unknown kpi key")
	// ErrUnavailable: the store could not answer. Show stale or empty; never crash.
	ErrUnavailable = errors.New("analytics: kpi service unavailable")
)

// Params is a KPI query. Structured only — no SQL crosses this API in either direction,
// which is the invariant that keeps the store swappable and the callers honest.
type Params struct {
	Grain    string    // hour|day|week|month|quarter|year. Default: day.
	From, To time.Time // zero → the store's default window
	Limit    int       // max points in Series
}

// Point is one bucket of a KPI series.
type Point struct {
	PeriodStart time.Time
	Value       float64
	Target      *float64
}

// Result is one KPI as a dashboard needs it: the definition metadata that says how to
// render it, the current and prior buckets, and the series behind them.
type Result struct {
	Key, Name, Area, Unit, Format, Direction, Definition string
	Version                                              int
	Grain                                                string
	Current, Prior                                       *Point
	DeltaPct                                             *float64
	OnTarget                                             *bool
	Series                                               []Point
	Stale                                                bool // served from cache after a failed refresh
}

type cacheEntry struct {
	res *Result
	at  time.Time
}

// KPI reads one KPI by name.
//
// The deadline is 3s by default and MUST stay under the host's MYCEL_HANDLE_TIMEOUT (5s):
// a read that uses the full host budget guarantees the host cancels first, so the caller
// sees a 504 instead of the degraded empty state this whole path is designed to produce.
//
// On failure a cached-but-expired result is returned with Stale set, rather than an
// error — a blip should age the number, not blank the screen.
func (c *Client) KPI(ctx context.Context, key string, p Params) (*Result, error) {
	if c == nil || c.disabled || c.cfg.Transport == nil {
		return nil, ErrNotConfigured
	}
	if p.Grain == "" {
		p.Grain = "day"
	}
	ck := key + "|" + p.canon()

	c.cacheMu.RLock()
	ent, ok := c.cache[ck]
	c.cacheMu.RUnlock()
	if ok && time.Since(ent.at) < c.cacheTTL {
		return ent.res, nil
	}

	rctx, cancel := context.WithTimeout(ctx, c.readTO)
	defer cancel()

	// Query string goes in the path: the transport carries no query field, and the
	// receiving side parses the full target.
	path := kpiPath + url.PathEscape(key) + "?" + p.encode()
	status, out, configured, err := c.cfg.Transport(rctx, c.envKey, "GET", path, nil)
	switch {
	case !configured:
		return nil, ErrNotConfigured
	case err == nil && status == 404:
		return nil, ErrNotFound
	case err == nil && status >= 200 && status < 300:
		var wire kpiResponse
		if uerr := json.Unmarshal(out, &wire); uerr != nil {
			return staleOr(ent, ok, ErrUnavailable)
		}
		res := wire.result()
		c.cacheMu.Lock()
		c.cache[ck] = cacheEntry{res: res, at: time.Now()}
		c.cacheMu.Unlock()
		return res, nil
	}
	return staleOr(ent, ok, ErrUnavailable)
}

// staleOr prefers an old number over no number: on a store blip a dashboard showing a
// 45-second-old figure with a stale marker is strictly more useful than a blank card.
func staleOr(ent cacheEntry, ok bool, err error) (*Result, error) {
	if ok && ent.res != nil {
		stale := *ent.res
		stale.Stale = true
		return &stale, nil
	}
	return nil, err
}

func (r kpiResponse) result() *Result {
	out := &Result{
		Key: r.Key, Name: r.Name, Area: r.Area, Unit: r.Unit, Format: r.Format,
		Direction: r.Direction, Definition: r.Definition, Version: r.Version,
		Grain: r.Grain, DeltaPct: r.DeltaPct, OnTarget: r.OnTarget,
	}
	if r.Current != nil {
		p := r.Current.point()
		out.Current = &p
	}
	if r.Prior != nil {
		p := r.Prior.point()
		out.Prior = &p
	}
	for _, s := range r.Series {
		out.Series = append(out.Series, s.point())
	}
	return out
}

func (p Params) encode() string {
	v := url.Values{}
	v.Set("grain", p.Grain)
	if !p.From.IsZero() {
		v.Set("from", p.From.UTC().Format(time.RFC3339))
	}
	if !p.To.IsZero() {
		v.Set("to", p.To.UTC().Format(time.RFC3339))
	}
	if p.Limit > 0 {
		v.Set("limit", strconv.Itoa(p.Limit))
	}
	return v.Encode()
}

// canon is the cache key: url.Values.Encode sorts keys, so it is already stable.
func (p Params) canon() string { return p.encode() }
