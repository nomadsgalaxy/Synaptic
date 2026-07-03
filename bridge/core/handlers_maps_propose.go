// handlers_maps_propose.go — R13 redesign (v2.4.0b1): map proposals.
//
// POST /maps/propose          create a new proposed map from a tag/query/auto trigger
// POST /maps/{id}/accept      flip a proposed map to status='accepted'
// POST /maps/{id}/dismiss     flip a proposed map to status='dismissed'
// GET  /maps?status=proposed  filter (already supported by mapsRoot list)
//
// Request shape:
//
//   POST /maps/propose
//   {
//     "source": "tag"|"query"|"recall_id"|"auto",   // required; trigger flavor
//     "tag":    "project-x",                         // when source=tag
//     "query":  "deep encoding",                     // when source=query
//     "memory_ids": ["..."],                         // when source=recall_id; explicit set
//     "name":   "Project X",                         // optional; LLM fills if blank
//     "type":   "project",                           // optional; default 'concept'
//     "limit":  20,                                  // max memories pulled into the proposal
//     "include_sensitive": false                     // per-call opt-in for Tier 2 synthesis
//   }
//
// Response:
//   {
//     "id": "...",                       // newly-created proposed map id
//     "status": "proposed",
//     "name": "...",                     // LLM-suggested or user-provided
//     "type": "...",
//     "anchor_tags": [...],
//     "schema_text": "...",              // synthesized prose
//     "source": "...",                   // back to user
//     "member_count": N,                 // memories the proposal binds to
//     "memories_excluded_by_privacy_filter": N,
//     "included_sensitive_count": N
//   }
//
// Privacy: sensitive memories are excluded from the synthesis prompt by
// default. include_sensitive=true forwards them to Tier 2 (note: Tier 2
// can be local or remote depending on the user's router config — when
// Tier 2 is local, the opt-in is effectively defensive transparency).
//
// Proposed maps are persisted with `status='proposed'` so they don't
// pollute the existing Maps tab until the user accepts. The frontend's
// "Proposed Maps" surface filters on `status=proposed`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

type proposeRequest struct {
	Source           string   `json:"source"`
	Tag              string   `json:"tag,omitempty"`
	Query            string   `json:"query,omitempty"`
	MemoryIDs        []string `json:"memory_ids,omitempty"`
	Name             string   `json:"name,omitempty"`
	Type             string   `json:"type,omitempty"`
	Limit            int      `json:"limit,omitempty"`
	IncludeSensitive bool     `json:"include_sensitive,omitempty"`
}

type proposeResponse struct {
	ID                              string   `json:"id"`
	Status                          string   `json:"status"`
	Name                            string   `json:"name"`
	Type                            string   `json:"type"`
	AnchorTags                      []string `json:"anchor_tags"`
	SchemaText                      string   `json:"schema_text"`
	Source                          string   `json:"source"`
	MemberCount                     int      `json:"member_count"`
	MemoriesExcludedByPrivacyFilter int      `json:"memories_excluded_by_privacy_filter"`
	IncludedSensitiveCount          int      `json:"included_sensitive_count"`
	// SynthesisFailed signals that the Tier 2 LLM call to generate the
	// map's name + schema couldn't complete (sidecar down, model not
	// pulled, timeout, etc.). The proposal still lands but with
	// placeholder text — caller should surface this so the user can
	// re-propose once Tier 2 is reachable for a real LLM synthesis.
	SynthesisFailed bool   `json:"synthesis_failed,omitempty"`
	SynthesisError  string `json:"synthesis_error,omitempty"`
}

// handleMapsPropose serves POST /maps/propose.
func (s *Server) handleMapsPropose(w http.ResponseWriter, r *http.Request) {
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
	if s.router == nil {
		http.Error(w, "router not configured", http.StatusServiceUnavailable)
		return
	}
	tier2 := s.router.Tier2()
	if tier2 == nil {
		http.Error(w, "Tier 2 (Nightly) provider unconfigured", http.StatusFailedDependency)
		return
	}

	var body proposeRequest
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch body.Source {
	case "tag", "query", "recall_id", "auto":
		// OK
	case "":
		http.Error(w, "source is required (tag|query|recall_id|auto)", http.StatusBadRequest)
		return
	default:
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}
	mapType := strings.TrimSpace(body.Type)
	if mapType == "" {
		mapType = "concept"
	}
	if !validMapTypes[mapType] {
		http.Error(w, "unknown type; expected project|person|technology|organization|concept", http.StatusBadRequest)
		return
	}
	limit := body.Limit
	if limit <= 0 {
		limit = 20
	}
	// 1000 ceiling — was 100, raised in v2.5.1b1 so a user with a
	// big OAuth/Auth/etc. tag can propose a map covering ALL matches
	// instead of an arbitrary top-100 slice. The Tier 2 synthesis
	// prompt is separately capped at proposeSynthesisMaxMembers (~80)
	// so context-window blowups don't happen even when the member
	// list runs into the hundreds; everything beyond that ceiling is
	// still linked as a member of the proposed map, just not included
	// in the synthesis prompt.
	if limit > 1000 {
		limit = 1000
	}

	// 1. Resolve the member set per source.
	mems, err := s.proposeResolveMembers(r.Context(), body, limit)
	if err != nil {
		http.Error(w, "resolve members: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(mems) == 0 {
		http.Error(w, "no memories matched the proposal trigger", http.StatusBadRequest)
		return
	}

	// 2. Privacy filter — drop sensitive memories from the Tier 2 prompt
	//    unless the caller opted in. Count is always surfaced (transparent).
	excluded := 0
	included := 0
	if !body.IncludeSensitive {
		filtered := mems[:0]
		for _, m := range mems {
			if m.Sensitive {
				excluded++
				continue
			}
			filtered = append(filtered, m)
		}
		mems = filtered
	} else {
		for _, m := range mems {
			if m.Sensitive {
				included++
			}
		}
	}
	if len(mems) == 0 {
		http.Error(w, "all candidate memories were sensitive — nothing to propose", http.StatusBadRequest)
		return
	}

	// 2b. Minimum-traces gate — applies ONLY to system-triggered ("auto")
	//     proposals so the nightly Phase 4 tag-cluster scan doesn't spam
	//     the Proposed queue with single-tag noise. User-initiated
	//     proposals (source=tag, query, recall_id) are deliberately
	//     ungated: if you click "+ Propose Map" you know what you're
	//     building and can populate it further later.
	//
	//     Phase 4 itself enforces the threshold one level earlier in
	//     runPhase4Maps (via pc.settings.MapProposalMinTraces) — this is
	//     the defence-in-depth check for any future direct /maps/propose
	//     call with source=auto.
	if body.Source == "auto" {
		if settings, sErr := s.bank.GetDreamPipelineSettings(); sErr == nil && settings.MapProposalMinTraces > 0 {
			if len(mems) < settings.MapProposalMinTraces {
				http.Error(w, fmt.Sprintf(
					"auto-proposal needs at least %d memories; trigger matched %d. (Manual proposals via tag/query/recall_id are not subject to this gate.) Lower the threshold in Config → Dreams (map_proposal_min_traces) to allow smaller auto-proposals.",
					settings.MapProposalMinTraces, len(mems)),
					http.StatusBadRequest)
				return
			}
		}
	}

	// 3. Tier 2 synthesis: ask the model for {name?, anchor_tags, schema_text}.
	suggested := proposeSuggest{Name: body.Name, Type: mapType}
	var synthesisFailed bool
	var synthesisErrMsg string
	if synth, sErr := s.proposeSynthesize(r.Context(), tier2, body, mems); sErr == nil {
		// Caller's name takes precedence; LLM fills if blank.
		if suggested.Name == "" {
			suggested.Name = synth.Name
		}
		suggested.AnchorTags = synth.AnchorTags
		suggested.SchemaText = synth.SchemaText
	} else {
		log.Printf("maps/propose: synthesis failed (%v); falling back to anchor-tag-only proposal", sErr)
		synthesisFailed = true
		// Trim the error for the response — full stack-traces include
		// internal paths/host names that we don't need to expose.
		synthesisErrMsg = sErr.Error()
		if len(synthesisErrMsg) > 200 {
			synthesisErrMsg = synthesisErrMsg[:200] + "…"
		}
		// Fallback: derive anchor tags from member tag frequency.
		suggested.AnchorTags = topAnchorTags(mems, 5)
		suggested.SchemaText = fmt.Sprintf("Auto-proposed from %d memories. Tier 2 synthesis unavailable — placeholder schema. Re-propose once Tier 2 (Nightly) is reachable to get a real LLM-generated name + schema.", len(mems))
	}
	if suggested.Name == "" {
		suggested.Name = fallbackProposalName(body, suggested.AnchorTags)
	}

	// 4. Persist as a proposed map.
	proposed := MemoryMap{
		Name:            suggested.Name,
		Type:            mapType,
		AnchorTags:      suggested.AnchorTags,
		SchemaText:      suggested.SchemaText,
		Source:          "inferred",
		Status:          "proposed",
		GeneratedBy:     proposeGeneratedByForSource(body.Source),
		GenerationPhase: proposePhaseForSource(body.Source),
	}
	saved, err := s.bank.SaveMemoryMap(proposed)
	if err != nil {
		http.Error(w, "save map: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 5. Bind member memories to the proposal via the existing trace-map
	//    join table so the dashboard can show "N memories in this proposal."
	for _, m := range mems {
		if err := s.bank.LinkMapTrace(saved.ID, m.ID); err != nil {
			log.Printf("maps/propose: LinkMapTrace %s -> %s: %v", saved.ID, m.ID, err)
		}
	}

	// 6. Audit + WS broadcast.
	s.auditWrite(AuditEntry{
		Operation:  "map_proposed",
		EntityType: "memory_map",
		EntityID:   saved.ID,
		AfterJSON: mustJSON(map[string]interface{}{
			"source":             body.Source,
			"member_count":       len(mems),
			"anchor_tags":        suggested.AnchorTags,
			"sensitive_excluded": excluded,
			"sensitive_included": included,
			"generated_by":       proposed.GeneratedBy,
		}),
		AdapterID: adapterIDFromRequest(r),
		Reason:    "user/system proposed a new map",
	})
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "map_proposed",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-maps",
			Payload: map[string]interface{}{
				"map_id":       saved.ID,
				"name":         saved.Name,
				"member_count": len(mems),
				"source":       body.Source,
			},
		})
	}

	writeJSON(w, http.StatusOK, proposeResponse{
		ID:                              saved.ID,
		Status:                          saved.Status,
		Name:                            saved.Name,
		Type:                            saved.Type,
		AnchorTags:                      saved.AnchorTags,
		SchemaText:                      saved.SchemaText,
		Source:                          body.Source,
		MemberCount:                     len(mems),
		SynthesisFailed:                 synthesisFailed,
		SynthesisError:                  synthesisErrMsg,
		MemoriesExcludedByPrivacyFilter: excluded,
		IncludedSensitiveCount:          included,
	})
}

// handleMapsCleanup serves POST /maps/cleanup. Bulk-deletes maps matching
// the filter set; supports a dry_run mode so the user can preview the count
// + sample names before committing. Cleanup is needed because the bank
// accumulates auto-generated tag-cluster concept maps from earlier
// pipelines that now produce nothing but trace_count=0 noise.
func (s *Server) handleMapsCleanup(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		DeleteEmpty       bool   `json:"delete_empty"`
		DeleteDismissed   bool   `json:"delete_dismissed"`
		DeleteAllMatching bool   `json:"delete_all_matching"`
		OnlyType          string `json:"only_type"`
		OnlyGeneratedBy   string `json:"only_generated_by"`
		DryRun            bool   `json:"dry_run"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	// Default: dry-run when no filters set to prevent accidental nukes.
	if !body.DeleteEmpty && !body.DeleteDismissed && !body.DeleteAllMatching {
		http.Error(w, "at least one of delete_empty / delete_dismissed / delete_all_matching must be true", http.StatusBadRequest)
		return
	}
	// delete_all_matching requires a scope filter — otherwise it would
	// nuke every map in the bank. Bank-side guard catches this too,
	// but we 400 here for a clearer error response.
	if body.DeleteAllMatching && body.OnlyType == "" && body.OnlyGeneratedBy == "" {
		http.Error(w, "delete_all_matching requires only_type or only_generated_by to scope the wipe", http.StatusBadRequest)
		return
	}
	result, err := s.bank.CleanupMemoryMaps(MapCleanupOpts{
		DeleteEmpty:       body.DeleteEmpty,
		DeleteDismissed:   body.DeleteDismissed,
		DeleteAllMatching: body.DeleteAllMatching,
		OnlyType:          body.OnlyType,
		OnlyGeneratedBy:   body.OnlyGeneratedBy,
		DryRun:            body.DryRun,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Audit only on real deletes — a dry-run shouldn't pollute the audit log.
	if !body.DryRun && result.Deleted > 0 {
		s.auditWrite(AuditEntry{
			Operation:  "memory_maps_cleanup",
			EntityType: "memory_map",
			AfterJSON: fmt.Sprintf(`{"deleted":%d,"eligible":%d,"filters":{"delete_empty":%t,"delete_dismissed":%t,"delete_all_matching":%t,"only_type":%q,"only_generated_by":%q}}`,
				result.Deleted, result.Eligible, body.DeleteEmpty, body.DeleteDismissed, body.DeleteAllMatching, body.OnlyType, body.OnlyGeneratedBy),
			AdapterID: adapterIDFromRequest(r),
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleMapAcceptDismiss serves POST /maps/{id}/accept | /maps/{id}/dismiss.
func (s *Server) handleMapAcceptDismiss(w http.ResponseWriter, r *http.Request, id string, action string) {
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
	var newStatus string
	switch action {
	case "accept":
		newStatus = "accepted"
	case "dismiss":
		newStatus = "dismissed"
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	before, err := s.bank.GetMemoryMap(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if before.Status != "proposed" {
		http.Error(w, fmt.Sprintf("map status is %q; only proposed maps can be %sed", before.Status, action), http.StatusConflict)
		return
	}
	updated, err := s.bank.UpdateMemoryMap(id, MemoryMapUpdate{Status: &newStatus})
	if err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 2026-05-21 — backfill map_traces on accept when the map has anchor_tags
	// but no traces. Phase 4 propose-time linking (Iter 25, 2026-05-14) only
	// covers maps proposed after that fix shipped; older proposed maps still
	// have empty trace lists and would render as empty after accept (the
	// `map-1778725082855342587` bug). LinkMapTrace is idempotent.
	if action == "accept" && len(before.AnchorTags) > 0 {
		existing, err := s.bank.ListTracesForMap(id, 1)
		if err == nil && len(existing) == 0 {
			tag := strings.TrimSpace(before.AnchorTags[0])
			if tag != "" {
				// Pull live memories sharing the anchor tag — same algorithm
				// proposeResolveMembers uses for source=tag.
				all, lerr := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 5000})
				if lerr == nil {
					tagLow := strings.ToLower(tag)
					linked := 0
					for _, m := range all {
						for _, t := range m.Tags {
							if strings.ToLower(strings.TrimSpace(t)) == tagLow {
								if lerr := s.bank.LinkMapTrace(id, m.ID); lerr == nil {
									linked++
								}
								break
							}
						}
					}
					if linked > 0 {
						log.Printf("map accept: backfilled %d trace link(s) for %s (anchor_tag=%q)", linked, id, tag)
					}
				}
			}
		}
	}
	s.auditWrite(AuditEntry{
		Operation:  "map_" + action,
		EntityType: "memory_map",
		EntityID:   id,
		BeforeJSON: mustJSON(before),
		AfterJSON:  mustJSON(updated),
		AdapterID:  adapterIDFromRequest(r),
	})
	s.emitMapUpdated(id, []string{"status"})
	writeJSON(w, http.StatusOK, updated)
}

// proposeSuggest is what the Tier 2 LLM call returns (parsed).
type proposeSuggest struct {
	Name       string   `json:"name"`
	Type       string   `json:"type,omitempty"`
	AnchorTags []string `json:"anchor_tags"`
	SchemaText string   `json:"schema_text"`
}

// proposeResolveMembers turns the proposal trigger into a member memory set.
func (s *Server) proposeResolveMembers(ctx context.Context, body proposeRequest, limit int) ([]MemoryRecord, error) {
	switch body.Source {
	case "tag":
		tag := strings.TrimSpace(body.Tag)
		if tag == "" {
			return nil, errors.New("source=tag requires `tag`")
		}
		// MemoryListOpts has no tag filter; pull a large page + filter in-Go.
		// Cap at 500 to avoid pulling the whole bank for users with N>500.
		all, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 500})
		if err != nil {
			return nil, err
		}
		tagLow := strings.ToLower(tag)
		out := make([]MemoryRecord, 0, limit)
		for _, m := range all {
			match := false
			for _, t := range m.Tags {
				if strings.ToLower(strings.TrimSpace(t)) == tagLow {
					match = true
					break
				}
			}
			if match {
				out = append(out, m)
				if len(out) >= limit {
					break
				}
			}
		}
		return out, nil
	case "recall_id":
		if len(body.MemoryIDs) == 0 {
			return nil, errors.New("source=recall_id requires `memory_ids`")
		}
		out := make([]MemoryRecord, 0, len(body.MemoryIDs))
		for i, id := range body.MemoryIDs {
			if i >= limit {
				break
			}
			rec, err := s.bank.GetMemory(id)
			if err == nil && rec.DeletedAt == "" {
				out = append(out, rec)
			}
		}
		return out, nil
	case "query":
		q := strings.TrimSpace(body.Query)
		if q == "" {
			return nil, errors.New("source=query requires `query`")
		}
		// Run the same Tier 1 embedding + cosine ranking /reflect uses.
		// Route through the router so this works with any provider kind
		// (Ollama, openai_local, …) and stays consistent with /recall.
		embedProvider := s.router.ForEmbedding()
		if embedProvider == nil {
			return nil, errors.New("propose: embedding provider not configured")
		}
		embedModel := providerModelLabel(embedProvider)
		queryVec, err := recallEmbedQuery(ctx, s.bank, q, embedProvider)
		if err != nil {
			return nil, err
		}
		memVecs, err := s.bank.AllEmbeddingsForMemories(embedModel)
		if err != nil {
			return nil, err
		}
		type scored struct {
			id    string
			score float64
		}
		scores := make([]scored, 0, len(memVecs))
		for id, vec := range memVecs {
			scores = append(scores, scored{id: id, score: recallScore(queryVec, vec)})
		}
		sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
		out := make([]MemoryRecord, 0, limit)
		for i := 0; i < len(scores) && len(out) < limit; i++ {
			rec, err := s.bank.GetMemory(scores[i].id)
			if err == nil && rec.DeletedAt == "" {
				out = append(out, rec)
			}
		}
		return out, nil
	case "auto":
		// System-triggered. Phase 8.7 rewire will populate body.MemoryIDs
		// before calling internally, so this path mostly mirrors recall_id.
		// User-facing direct "auto" requests are accepted but require ids.
		if len(body.MemoryIDs) == 0 {
			return nil, errors.New("source=auto requires `memory_ids` (Phase 8.7 should populate)")
		}
		return s.proposeResolveMembers(ctx, proposeRequest{Source: "recall_id", MemoryIDs: body.MemoryIDs}, limit)
	}
	return nil, errors.New("unsupported source")
}

// proposeSynthesisMaxMembers caps how many member memories we include
// in the Tier 2 synthesis prompt. Members beyond this slice are STILL
// linked to the proposed map (proposeResolveMembers returns the full
// set + LinkMapTrace iterates over it) — they just don't influence the
// name/schema generation. The Tier 2 model has a finite context window
// (~32k–128k tokens depending on the model) and each memory takes up to
// ~300 chars × ~0.75 tokens/char = ~225 tokens of prompt. 80 memories
// = ~18k tokens of member text + prompt overhead, leaving plenty of
// headroom on llama3.1:8b and any modern remote model.
const proposeSynthesisMaxMembers = 80

// proposeSynthesize asks Tier 2 to suggest a name + anchor tags + schema_text
// from the member memory set. Best-effort: callers fall back to anchor-tag
// extraction when this errors.
func (s *Server) proposeSynthesize(ctx context.Context, tier2 LLMProvider, body proposeRequest, mems []MemoryRecord) (proposeSuggest, error) {
	// Use only the top N for synthesis — members are already
	// sorted by relevance (cosine score for source=query, insertion
	// order for tag/recall_id). Past this slice the prompt grows
	// faster than the model can usefully consume.
	synthMems := mems
	if len(synthMems) > proposeSynthesisMaxMembers {
		synthMems = synthMems[:proposeSynthesisMaxMembers]
	}
	prompt := buildProposePrompt(body, synthMems)
	nctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	out, err := tier2.Chat(nctx, []Message{{Role: "user", Content: prompt}}, 600)
	if err != nil {
		return proposeSuggest{}, err
	}
	// Forgiving parser: try strict JSON first, fall back to extracting
	// fields with simple regex if Tier 2 wrapped the JSON in prose.
	var parsed proposeSuggest
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); jerr == nil && (parsed.Name != "" || len(parsed.AnchorTags) > 0) {
		return parsed, nil
	}
	// Try to find a JSON object inside the response.
	if start := strings.Index(out, "{"); start >= 0 {
		if end := strings.LastIndex(out, "}"); end > start {
			candidate := out[start : end+1]
			if jerr := json.Unmarshal([]byte(candidate), &parsed); jerr == nil {
				return parsed, nil
			}
		}
	}
	// Loose parse: use the response as schema_text + derive tags ourselves.
	return proposeSuggest{
		Name:       "",
		AnchorTags: topAnchorTags(mems, 5),
		SchemaText: strings.TrimSpace(out),
	}, nil
}

func buildProposePrompt(body proposeRequest, mems []MemoryRecord) string {
	var b strings.Builder
	b.WriteString(`You are proposing a new MemoryMap — a thematic grouping of memories the user can review, accept, edit, or dismiss. Look at the memory set below and return JSON only, with these fields:

{
  "name": "...",          // 2-5 word title (concise; nouny; user will see this)
  "anchor_tags": ["..."], // 3-6 lowercase tags that capture the theme
  "schema_text": "..."    // one short paragraph (2-4 sentences) describing what binds these memories together, written for the user
}

The user has NOT yet accepted this proposal — frame the schema_text as a hypothesis, not a fact ("These memories appear to share..." rather than "This is..."). Do NOT include statistics, IDs, or counts.`)
	b.WriteString("\n\nTRIGGER: ")
	b.WriteString(strings.ToUpper(body.Source))
	if body.Tag != "" {
		b.WriteString(" — tag=")
		b.WriteString(body.Tag)
	}
	if body.Query != "" {
		b.WriteString(" — query=")
		b.WriteString(strings.ReplaceAll(body.Query, "\n", " "))
	}
	b.WriteString("\n\nMEMBER MEMORIES:\n")
	for i, m := range mems {
		text := m.EnrichedText
		if text == "" {
			text = m.Text
		}
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, text)
	}
	b.WriteString("\nReturn JSON only.")
	return b.String()
}

// topAnchorTags returns the N most frequent tags across the member set.
func topAnchorTags(mems []MemoryRecord, n int) []string {
	counts := map[string]int{}
	for _, m := range mems {
		for _, t := range m.Tags {
			t = strings.ToLower(strings.TrimSpace(t))
			if t != "" {
				counts[t]++
			}
		}
	}
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	out := make([]string, 0, n)
	for i := 0; i < len(pairs) && i < n; i++ {
		out = append(out, pairs[i].k)
	}
	return out
}

func fallbackProposalName(body proposeRequest, anchorTags []string) string {
	if body.Tag != "" {
		return "Proposed: " + body.Tag
	}
	if body.Query != "" {
		q := body.Query
		if len(q) > 40 {
			q = q[:40] + "…"
		}
		return "Proposed: " + q
	}
	if len(anchorTags) > 0 {
		return "Proposed: " + anchorTags[0]
	}
	return "Proposed Map"
}

func proposeGeneratedByForSource(source string) string {
	switch source {
	case "auto":
		return "r13_redesign"
	default:
		return "user"
	}
}

func proposePhaseForSource(source string) string {
	if source == "auto" {
		return "8.7"
	}
	return "user_triggered"
}
