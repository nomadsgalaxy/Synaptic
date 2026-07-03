// live_neighbor_stream.go — real-time semantic neighbor surfacing.
//
// Whenever a hook event arrives with substantive text content, embed
// the text and find the top-K most-similar memories in the bank. If
// any cross a similarity threshold, broadcast a `memory_neighbors_found`
// WS event so the dashboard can show "N prior memories about this"
// next to the live activity feed.
//
// Cost model:
//   - One embedding call per qualifying event (Tier Embed, local Ollama,
//     ~30ms locally). High-volume event types (bubble updates, ws
//     heartbeats) are filtered out so the rate stays manageable.
//   - One cosine pass over the cached embeddings map. O(N×768) for
//     N memories; ~5ms for a 3k-memory bank.
//
// Default behaviour: OFF. Opt-in via the `live_neighbor_stream_enabled`
// setting so the embed traffic is explicit.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	LiveNeighborStreamEnabledKey   = "live_neighbor_stream_enabled"
	LiveNeighborStreamThresholdKey = "live_neighbor_stream_threshold" // cosine min (default 0.70)
	LiveNeighborStreamTopKKey      = "live_neighbor_stream_top_k"     // top-K to emit (default 3)
	LiveNeighborStreamMinCharsKey  = "live_neighbor_stream_min_chars" // skip short events (default 40)
)

// liveNeighborEventTypes whitelists event types that carry meaningful
// text worth embedding. Tool calls + memory writes + reflection are
// the high-signal ones; raw model_thinking + ws heartbeats are skipped.
var liveNeighborEventTypes = map[string]bool{
	"prompt_received":  true,
	"response_complete": true,
	"tool_call":        true,
	"tool_result":      true,
	"memory_added":     true,
	"memory_recall":    true,
}

// extractEventText pulls the embeddable string out of a hook event's
// payload. Returns "" when nothing useful is present (caller skips).
func extractEventText(payload map[string]interface{}) string {
	for _, key := range []string{"text", "content", "query", "prompt", "summary"} {
		if v, ok := payload[key].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// liveNeighborSettings reads the four knobs from the bank with sane defaults.
type liveNeighborSettings struct {
	Enabled   bool
	Threshold float64
	TopK      int
	MinChars  int
}

func loadLiveNeighborSettings(bank *Bank) liveNeighborSettings {
	out := liveNeighborSettings{Enabled: false, Threshold: 0.70, TopK: 3, MinChars: 40}
	if bank == nil {
		return out
	}
	if v, ok, _ := bank.GetSetting(LiveNeighborStreamEnabledKey); ok && v == "true" {
		out.Enabled = true
	}
	if v, ok, _ := bank.GetSetting(LiveNeighborStreamThresholdKey); ok && v != "" {
		if f := parseFloatOr(v, out.Threshold); f > 0 && f <= 1 {
			out.Threshold = f
		}
	}
	if v, ok, _ := bank.GetSetting(LiveNeighborStreamTopKKey); ok && v != "" {
		if n := parseIntOr(v, out.TopK); n > 0 && n <= 20 {
			out.TopK = n
		}
	}
	if v, ok, _ := bank.GetSetting(LiveNeighborStreamMinCharsKey); ok && v != "" {
		if n := parseIntOr(v, out.MinChars); n > 0 {
			out.MinChars = n
		}
	}
	return out
}

// emitLiveNeighbors is fired from postEvent's persist branch. Returns
// immediately when disabled or the event doesn't carry embeddable text.
// Runs the actual embed + neighbor search in a background goroutine so
// the /event response isn't blocked.
func (s *Server) emitLiveNeighbors(parentCtx context.Context, e Event) {
	if s == nil || s.bank == nil || s.hub == nil || s.router == nil {
		return
	}
	if !liveNeighborEventTypes[e.Type] {
		return
	}
	settings := loadLiveNeighborSettings(s.bank)
	if !settings.Enabled {
		return
	}
	text := extractEventText(e.Payload)
	if len(text) < settings.MinChars {
		return
	}
	embedder := s.router.ForEmbedding()
	if embedder == nil {
		return
	}
	// Detach from the request context — the search may outlive the
	// HTTP response and we don't want to cancel it on connection close.
	go func(text string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vec, err := embedder.Embed(ctx, text)
		if err != nil || len(vec) == 0 {
			return
		}
		model := embedder.Name()
		all, err := s.bank.AllEmbeddingsForMemories(model)
		if err != nil || len(all) == 0 {
			return
		}
		type scored struct {
			id  string
			sim float64
		}
		scoredList := make([]scored, 0, len(all))
		for id, v := range all {
			scoredList = append(scoredList, scored{id: id, sim: float64(vecDot(vec, v))})
		}
		sort.Slice(scoredList, func(i, j int) bool { return scoredList[i].sim > scoredList[j].sim })
		if len(scoredList) == 0 || scoredList[0].sim < settings.Threshold {
			return // nothing close enough — stay quiet rather than show noise
		}
		k := settings.TopK
		if k > len(scoredList) {
			k = len(scoredList)
		}
		matches := make([]map[string]interface{}, 0, k)
		for i := 0; i < k; i++ {
			if scoredList[i].sim < settings.Threshold {
				break
			}
			rec, gerr := s.bank.GetMemory(scoredList[i].id)
			if gerr != nil {
				continue
			}
			preview := rec.Text
			if len(preview) > 120 {
				preview = preview[:120]
			}
			matches = append(matches, map[string]interface{}{
				"memory_id":  rec.ID,
				"similarity": scoredList[i].sim,
				"preview":    preview,
				"region":     rec.RegionHint,
				"tags":       rec.Tags,
			})
		}
		if len(matches) == 0 {
			return
		}
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "memory_neighbors_found",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-neighbors",
			SessionID:     e.SessionID,
			Payload: map[string]interface{}{
				"source_event_type": e.Type,
				"source_text":       truncateForNeighbor(text, 200),
				"matches":           matches,
				"threshold":         settings.Threshold,
			},
		})
	}(text)
}

func parseFloatOr(s string, def float64) float64 {
	var f float64
	if _, err := fmt.Sscan(s, &f); err == nil {
		return f
	}
	return def
}

func parseIntOr(s string, def int) int {
	var n int
	if _, err := fmt.Sscan(s, &n); err == nil {
		return n
	}
	return def
}

func truncateForNeighbor(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
