package analytics

import (
	"encoding/json"
	"time"
)

// The two routes on the kpi module. Paths, not constants shared by import: this package
// and mycel_kpi agree by contract (documented in kpi_and_analytics.md), not by a Go
// dependency, because a module in another language must be able to speak the same wire.
const (
	eventsPath = "/events"
	kpiPath    = "/kpi/"
)

// batchBody is the POST /events payload.
//
// Service, Version and Counters live in the BODY, not in headers, because the
// module→module transport carries only method, path and body — headers and query string
// are dropped on that hop. The counters ride along on every batch so the store can record
// them as KPIs, which is how a drop becomes visible on a dashboard without any metrics
// pipeline existing in the fabric.
type batchBody struct {
	Service  string      `json:"service"`
	Version  string      `json:"version,omitempty"`
	Counters counters    `json:"counters"`
	Events   []wireEvent `json:"events"`
}

type counters struct {
	Emitted  uint64 `json:"emitted"`
	Dropped  uint64 `json:"dropped"`
	Rejected uint64 `json:"rejected"`
}

// wireEvent is the on-the-wire shape of one event. Field names are snake_case and match
// the ClickHouse column names one-for-one, so the store never has to translate.
type wireEvent struct {
	EventType   string             `json:"event_type"`
	EventID     string             `json:"event_id"`
	OccurredAt  string             `json:"occurred_at"` // RFC3339 with millis, UTC
	SubjectType string             `json:"subject_type"`
	SubjectID   string             `json:"subject_id"`
	ActorID     string             `json:"actor_id,omitempty"`
	TraceID     string             `json:"trace_id,omitempty"`
	Attrs       map[string]string  `json:"attrs,omitempty"`
	Nums        map[string]float64 `json:"nums,omitempty"`
}

func (c *Client) encodeBatch(batch []Event) ([]byte, error) {
	out := batchBody{
		Service:  c.cfg.Service,
		Version:  c.cfg.Version,
		Counters: counters{Emitted: c.emitted.Load(), Dropped: c.dropped.Load(), Rejected: c.rejected.Load()},
		Events:   make([]wireEvent, 0, len(batch)),
	}
	for _, e := range batch {
		out.Events = append(out.Events, wireEvent{
			EventType:   e.Type,
			EventID:     e.ID,
			OccurredAt:  e.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			SubjectType: e.SubjectType,
			SubjectID:   e.SubjectID,
			ActorID:     e.ActorID,
			TraceID:     e.TraceID,
			Attrs:       e.Attrs,
			Nums:        e.Nums,
		})
	}
	return json.Marshal(out)
}

// kpiResponse is the GET /kpi/{key} payload.
type kpiResponse struct {
	Key        string      `json:"key"`
	Name       string      `json:"name"`
	Area       string      `json:"area"`
	Unit       string      `json:"unit"`
	Format     string      `json:"format"`
	Direction  string      `json:"direction"`
	Definition string      `json:"definition"`
	Version    int         `json:"version"`
	Grain      string      `json:"grain"`
	Current    *wirePoint  `json:"current"`
	Prior      *wirePoint  `json:"prior"`
	DeltaPct   *float64    `json:"delta_pct"`
	OnTarget   *bool       `json:"on_target"`
	Series     []wirePoint `json:"series"`
}

type wirePoint struct {
	PeriodStart string   `json:"period_start"`
	Value       float64  `json:"value"`
	Target      *float64 `json:"target,omitempty"`
}

func (p wirePoint) point() Point {
	t, _ := time.Parse(time.RFC3339, p.PeriodStart)
	return Point{PeriodStart: t, Value: p.Value, Target: p.Target}
}
