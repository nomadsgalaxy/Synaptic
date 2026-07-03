// hook_events.go — Wave 5 hook log feature.
//
// Persisted stream of forwarded hook/adapter events (Claude Code hooks,
// MCP server, OTel collector). Distinct from audit_log: audit_log
// captures *Synaptic's* state changes (create/update/delete + nightly
// steps); hook_events captures *upstream adapter signals* so the user
// can correlate "what was the agent doing right before this memory got
// saved" or "what hook fired during this nightly run."
//
// Endpoint: POST /events (write), GET /events (filtered read).
//
// Retention: rows are kept indefinitely by default; the user can prune
// via SD_HOOK_EVENT_RETENTION_DAYS env or `DELETE FROM hook_events WHERE
// created_at < ...`. Default cap on payload size = 2KB so a misbehaving
// adapter can't bloat the DB.
package main

import (
	"errors"
	"strings"
	"time"
)

// MaxHookEventPayloadBytes truncates oversized event bodies at write
// time. 2KB matches the spec from the wave-5 brief.
const MaxHookEventPayloadBytes = 2048

// HookEvent is one persisted row.
type HookEvent struct {
	ID        string `json:"id"`
	AdapterID string `json:"adapter_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	EventType string `json:"event_type"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// HookEventListOpts is the filter set for GET /events. Empty fields are
// not filtered. SinceID is exclusive; the API returns rows with id > since
// (sorted by created_at).
type HookEventListOpts struct {
	AdapterID string
	SessionID string
	EventType string
	Since     string // created_at > since (RFC3339)
	Limit     int    // default 100, cap 1000
}

// SaveHookEvent persists one event. ID is auto-generated when empty.
// Payload is truncated to MaxHookEventPayloadBytes — the suffix is
// replaced with "...<truncated>" so a reader can tell.
func (b *Bank) SaveHookEvent(ev HookEvent) (HookEvent, error) {
	if b == nil {
		return ev, errors.New("bank not enabled")
	}
	if ev.EventType == "" {
		return ev, errors.New("hook event: event_type is required")
	}
	if ev.ID == "" {
		ev.ID = newID("hook")
	}
	if ev.CreatedAt == "" {
		ev.CreatedAt = nowUTC()
	}
	if len(ev.Payload) > MaxHookEventPayloadBytes {
		// Keep the last bytes so JSON-shaped payloads remain partially
		// readable — but always end with a clear marker.
		const marker = "...<truncated>"
		ev.Payload = ev.Payload[:MaxHookEventPayloadBytes-len(marker)] + marker
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO hook_events (id, adapter_id, session_id, event_type, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.AdapterID, ev.SessionID, ev.EventType, ev.Payload, ev.CreatedAt)
	if err != nil {
		return ev, err
	}
	return ev, nil
}

// ListHookEvents returns rows matching the filter, newest-first.
func (b *Bank) ListHookEvents(opts HookEventListOpts) ([]HookEvent, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	conds := []string{"1=1"}
	args := []any{}
	if opts.AdapterID != "" {
		conds = append(conds, "adapter_id = ?")
		args = append(args, opts.AdapterID)
	}
	if opts.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.EventType != "" {
		conds = append(conds, "event_type = ?")
		args = append(args, opts.EventType)
	}
	if opts.Since != "" {
		conds = append(conds, "created_at > ?")
		args = append(args, opts.Since)
	}
	args = append(args, limit)
	q := `SELECT id, adapter_id, session_id, event_type, payload, created_at
	      FROM hook_events
	      WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY created_at DESC
	      LIMIT ?`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HookEvent{}
	for rows.Next() {
		var e HookEvent
		if err := rows.Scan(&e.ID, &e.AdapterID, &e.SessionID, &e.EventType, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneHookEvents deletes rows older than `cutoff`. Returns the number of
// rows removed. Use SD_HOOK_EVENT_RETENTION_DAYS for an automated cron.
func (b *Bank) PruneHookEvents(cutoff time.Time) (int64, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM hook_events WHERE created_at < ?`,
		cutoff.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// (The /events HTTP route is extended in main.go's getEvents +
// postEventsBatch — see HookEventListOpts usage there. Keeping the route
// glue in one place avoids registering a competing /events handler.)

