// handlers_lexicon_rebuild.go — manual lexicon-pairs rebuild trigger.
//
// The user's primary frustration (per the handoff): "Pairs seems to be
// empty, but I'm not sure if that is a bug, or if it's genuinely because
// the nightly consolidation didn't run." This endpoint lets them click
// "Rebuild now" and watch the table populate, distinguishing "broken"
// from "unbuilt" without having to dig through logs.
//
//   POST /lexicon/rebuild
//     Body: { "scope": "pairs"|"counts"|"all" }    (currently only "all"
//                                                   meaningful — counts are
//                                                   recomputed alongside
//                                                   pairs in the same scan)
//     202: { "run_id": "...", "scope": "all", "status": "queued" }
//     409: another rebuild is in flight; { "error": ..., "run_id": "..." }
//
// Concurrency: a sync.Mutex on the Server gates /lexicon/rebuild so a second
// trigger while one is running returns 409. Same pattern as the NightlyRunner.
//
// The actual rebuild runs in a goroutine — returns 202 immediately so the
// frontend can poll /lexicon to watch the row count grow.
//
// Algorithm:
//   1. Read all live (non-deleted) memories with tags.
//   2. For each memory, iterate each pair of distinct tags (canonicalised).
//   3. Increment the lexicon row's cooccurrence count by 1 per pair.
//
// We don't TRUNCATE first — the rebuild is additive. If counts drift from
// reality we can ship a "reset + rebuild" later. For MVP, additive is
// safer (no risk of losing data on a partial failure).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

func (s *Server) postLexiconRebuild(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}

	// Concurrency check: read the in-flight id atomically. If non-empty,
	// 409 the new trigger.
	if curID, _ := s.lexiconRebuildID.Load().(string); curID != "" {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":  "rebuild already in progress",
			"run_id": curID,
		})
		return
	}

	// TryLock so the response is fast and we can return 409 race-free if
	// another goroutine grabbed the mutex first.
	if !s.lexiconRebuildMu.TryLock() {
		curID, _ := s.lexiconRebuildID.Load().(string)
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":  "rebuild already in progress",
			"run_id": curID,
		})
		return
	}

	runID := fmt.Sprintf("lexicon-%d", nextMonotonicNano())
	s.lexiconRebuildID.Store(runID)

	go s.runLexiconRebuild(r.Context(), runID)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"run_id": runID,
		"scope":  "all",
		"status": "queued",
	})
}

// runLexiconRebuild walks all live memories and rebuilds the lexicon's
// pairwise cooccurrence counts. Runs as a goroutine triggered by
// postLexiconRebuild. Releases the mutex + clears the in-flight id when
// done so the next trigger can run.
func (s *Server) runLexiconRebuild(ctx context.Context, runID string) {
	startedAt := time.Now()
	defer func() {
		s.lexiconRebuildID.Store("")
		s.lexiconRebuildMu.Unlock()
	}()
	defer func() {
		// Panic safety: if the rebuild crashes, log + emit an error event
		// rather than tearing down the whole server.
		if rec := recover(); rec != nil {
			log.Printf("lexicon-rebuild: panic: %v", rec)
			if s.hub != nil {
				s.hub.FanoutEphemeral(Event{
					SchemaVersion: SchemaVersion,
					Type:          "lexicon_rebuild_done",
					Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
					AdapterID:     "sd-core-lexicon",
					Payload: map[string]interface{}{
						"run_id": runID,
						"error":  fmt.Sprintf("panic: %v", rec),
					},
				})
			}
		}
	}()

	// Pull live memories. Cap at 10K — a deliberate ceiling so a runaway
	// bank doesn't lock the rebuild for hours. If the user's bank exceeds
	// this, they'll get partial results plus a log line; future work can
	// paginate.
	mems, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 10000})
	if err != nil {
		log.Printf("lexicon-rebuild %s: list memories: %v", runID, err)
		return
	}
	log.Printf("lexicon-rebuild %s: scanning %d memories", runID, len(mems))

	pairsWritten := 0
	for i, m := range mems {
		if ctx.Err() != nil {
			log.Printf("lexicon-rebuild %s: cancelled at %d/%d", runID, i, len(mems))
			break
		}
		// IncrementLexicon canonicalises (a, b) order so duplicates
		// collapse to the same row. Self-pairs and empty tags are no-ops
		// in the bank method.
		for j := 0; j < len(m.Tags); j++ {
			for k := j + 1; k < len(m.Tags); k++ {
				if err := s.bank.IncrementLexicon(m.Tags[j], m.Tags[k]); err == nil {
					pairsWritten++
				}
			}
		}
		// Periodic progress events so the dashboard can render a bar.
		if s.hub != nil && i%500 == 499 {
			s.hub.FanoutEphemeral(Event{
				SchemaVersion: SchemaVersion,
				Type:          "lexicon_rebuild_progress",
				Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
				AdapterID:     "sd-core-lexicon",
				Payload: map[string]interface{}{
					"run_id": runID,
					"phase":  "pairs",
					"done":   i + 1,
					"total":  len(mems),
				},
			})
		}
	}

	dur := time.Since(startedAt)
	log.Printf("lexicon-rebuild %s: complete — %d pair-increments in %s",
		runID, pairsWritten, dur)

	// Emit the done event + audit row.
	if s.hub != nil {
		s.hub.FanoutEphemeral(Event{
			SchemaVersion: SchemaVersion,
			Type:          "lexicon_rebuild_done",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-lexicon",
			Payload: map[string]interface{}{
				"run_id": runID,
				"stats": map[string]interface{}{
					"pairs_written": pairsWritten,
					"memories":      len(mems),
					"duration_ms":   dur.Milliseconds(),
				},
			},
		})
	}
	_ = s.bank.AppendAudit(AuditEntry{
		Operation:  "lexicon_rebuild",
		EntityType: "lexicon",
		EntityID:   runID,
		AfterJSON: fmt.Sprintf(
			`{"pairs_written":%d,"memories":%d,"duration_ms":%d}`,
			pairsWritten, len(mems), dur.Milliseconds(),
		),
		AdapterID: "sd-core-lexicon",
	})
}

// silenceUnusedAtomicImport keeps `atomic` referenced if no other file in
// the package needs it for the lexicon rebuild path. Compile-time only.
var _ = atomic.Value{}
