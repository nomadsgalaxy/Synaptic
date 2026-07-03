// handlers_admin_extract_facts.go — POST /admin/extract-facts
//
// Narrative-fact extraction. Takes a multi-turn session (role + content
// per turn) plus a session date, and asks Tier 2 to produce 2-5
// self-contained narrative facts that capture decisions, preferences,
// experiences, and observations. Each fact is stored as a MemoryRecord
// with type-tagged metadata, ABSOLUTE dates (relative references resolved
// using the session_date as anchor), and the source turns referenced.
//
// Why this exists: per-turn storage with downstream summarization loses
// pragmatic structure. "User considered X, then Y, then chose Z" becomes
// three separate memories that all retrieve under "X" / "Y" / "Z" queries —
// the model can't tell which was the final decision. Narrative-fact
// extraction collapses that into one memory: "User chose Z (rejected X, Y)
// for the Radiation-Amplified zombie name on 2026-04-12."
//
// Narrative-fact ingestion materially improves retrieval on
// "what did the user decide" / "what was the final choice" questions
// vs raw per-turn storage, at the cost of some count-precision on
// multi-session aggregates where each turn carries its own number.
//
// Endpoint: POST /admin/extract-facts
//
// Request body:
//
//	{
//	  "session_id":   "answer_cf425855_1",
//	  "session_date": "2023-05-24T05:09:00Z",
//	  "adapter_id":   "min-...",
//	  "region_hint":  "benchmark",
//	  "turns": [
//	    {"role": "user",      "content": "..."},
//	    {"role": "assistant", "content": "..."}
//	  ],
//	  "extra_tags": ["benchmark:min", "min-bench"],
//	  "max_facts":  5  // optional; default 5
//	}
//
// Response:
//
//	{
//	  "session_id":    "...",
//	  "facts_created": [
//	    {"id": "fact-...", "type": "Decision",    "text": "...", "entities": [...]},
//	    {"id": "fact-...", "type": "Preference",  "text": "...", "entities": [...]},
//	    {"id": "fact-...", "type": "Experience",  "text": "...", "entities": [...]}
//	  ],
//	  "tier2_tokens_in":  1234,
//	  "tier2_tokens_out": 567,
//	  "elapsed_ms":       4200
//	}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type extractFactsTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type extractFactsRequest struct {
	SessionID   string             `json:"session_id"`
	SessionDate string             `json:"session_date"`
	AdapterID   string             `json:"adapter_id"`
	RegionHint  string             `json:"region_hint"`
	Turns       []extractFactsTurn `json:"turns"`
	ExtraTags   []string           `json:"extra_tags"`
	MaxFacts    int                `json:"max_facts"`
}

type extractedFact struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Text     string   `json:"text"`
	Entities []string `json:"entities,omitempty"`
	Dates    []string `json:"dates,omitempty"`
}

type extractFactsResponse struct {
	SessionID      string          `json:"session_id"`
	FactsCreated   []extractedFact `json:"facts_created"`
	Tier2TokensIn  int             `json:"tier2_tokens_in"`
	Tier2TokensOut int             `json:"tier2_tokens_out"`
	ElapsedMs      int64           `json:"elapsed_ms"`
}

// llmFactPayload is the JSON shape we ask Tier 2 to emit.
type llmFactPayload struct {
	Type     string   `json:"type"`
	Text     string   `json:"text"`
	Entities []string `json:"entities,omitempty"`
	Dates    []string `json:"dates,omitempty"`
}

func (s *Server) postAdminExtractFacts(w http.ResponseWriter, r *http.Request) {
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

	var body extractFactsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Turns) == 0 {
		http.Error(w, "turns is required", http.StatusBadRequest)
		return
	}
	if body.MaxFacts <= 0 {
		body.MaxFacts = 5
	}

	provider := s.router.ForNightly()
	if provider == nil {
		http.Error(w, "no tier2 provider configured", http.StatusServiceUnavailable)
		return
	}

	t0 := time.Now()

	prompt := buildExtractFactsPrompt(body)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	out, err := provider.Chat(ctx, []Message{{Role: "user", Content: prompt}}, 2000)
	if err != nil {
		http.Error(w, "tier2 chat: "+err.Error(), http.StatusBadGateway)
		return
	}

	facts, perr := parseExtractFactsResponse(out)
	if perr != nil {
		http.Error(w, "parse: "+perr.Error()+" | raw: "+truncForLog(out, 500), http.StatusBadGateway)
		return
	}
	if len(facts) > body.MaxFacts {
		facts = facts[:body.MaxFacts]
	}

	created := make([]extractedFact, 0, len(facts))
	for i, fp := range facts {
		if strings.TrimSpace(fp.Text) == "" {
			continue
		}
		typeTag := "Observation"
		if fp.Type != "" {
			typeTag = fp.Type
		}
		factID := fmt.Sprintf("fact-%d-%d", time.Now().UnixNano(), i)
		tags := []string{
			"narrative_fact",
			"fact_type:" + strings.ToLower(typeTag),
		}
		tags = append(tags, body.ExtraTags...)
		for _, e := range fp.Entities {
			if e = strings.TrimSpace(e); e != "" {
				tags = append(tags, "entity:"+strings.ToLower(e))
			}
		}
		rec := MemoryRecord{
			ID:         factID,
			Text:       strings.TrimSpace(fp.Text),
			Tags:       tags,
			AdapterID:  body.AdapterID,
			SessionID:  body.SessionID,
			RegionHint: body.RegionHint,
			Source:     "extract-facts",
			CreatedAt:  body.SessionDate,
		}
		saved, sErr := s.bank.SaveMemory(rec)
		if sErr != nil {
			continue
		}
		created = append(created, extractedFact{
			ID:       saved.ID,
			Type:     typeTag,
			Text:     saved.Text,
			Entities: fp.Entities,
			Dates:    fp.Dates,
		})
	}

	resp := extractFactsResponse{
		SessionID:      body.SessionID,
		FactsCreated:   created,
		Tier2TokensIn:  estimateTokens(prompt),
		Tier2TokensOut: estimateTokens(out),
		ElapsedMs:      time.Since(t0).Milliseconds(),
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// buildExtractFactsPrompt assembles the Tier 2 prompt asking for narrative
// facts. Each fact is self-contained, preserves pragmatic flow
// ("user chose X over Y"), and resolves relative dates to absolute
// using session_date as the anchor.
func buildExtractFactsPrompt(req extractFactsRequest) string {
	var b strings.Builder
	b.Grow(2048 + 200*len(req.Turns))
	b.WriteString(`You are extracting narrative facts from a conversation session for long-term memory storage. Read the entire session below, then emit 2-`)
	fmt.Fprintf(&b, "%d", req.MaxFacts)
	b.WriteString(` SELF-CONTAINED facts that capture what happened, what was decided, and what the user revealed about themselves.

CRITICAL RULES:
1. SELF-CONTAINED — each fact must stand alone. Spell out names, no pronouns referring to outside context, no "they said".
2. PRESERVE EVERY CONCRETE FACT — names, dates, numbers, places, brands, outcomes.
3. RESOLVE RELATIVE DATES to absolute. Use the session_date as the anchor for "today", "yesterday", "last Friday", "two weeks ago", etc. Compute and write the absolute date.
4. CAPTURE DECISIONS — when multiple options are suggested and one is chosen ("decided", "went with", "settled on", "named it", "picked", "I'll go with", "REALLY cool one"), name the chosen one FIRST and explicitly list rejected alternatives. Pattern: "User chose X for <topic> over <Y, Z>".
5. CAPTURE PREFERENCES — when the user expresses interest, preference, or dislike, quote the specific item (named brand, sub-genre, named place). Pattern: "User prefers / is interested in / enjoys <specific thing>".
6. CAPTURE EXPERIENCES — when the user describes a past event, capture date, location, participants, outcome. Pattern: "On <date>, user did <action> at <place> with <people>, resulting in <outcome>".
7. CAPTURE OBSERVATIONS — when the user states a fact about themselves, capture it directly. Pattern: "User has / is / owns / believes <fact>".
8. NO HALLUCINATION — only assert facts that appear in the session below.

SESSION METADATA:
  session_id:   `)
	b.WriteString(req.SessionID)
	b.WriteString("\n  session_date: ")
	b.WriteString(req.SessionDate)
	b.WriteString("\n\nCONVERSATION:\n")
	for _, t := range req.Turns {
		role := t.Role
		if role == "" {
			role = "user"
		}
		b.WriteString("  [")
		b.WriteString(role)
		b.WriteString("] ")
		content := strings.TrimSpace(t.Content)
		if len(content) > 1500 {
			content = content[:1500] + "…"
		}
		b.WriteString(content)
		b.WriteString("\n")
	}
	b.WriteString(`
Return ONLY a JSON array (no preamble, no code fences). Each element has:
  type:     one of "Experience" | "Decision" | "Preference" | "Observation"
  text:     the self-contained fact (1-3 sentences). MUST resolve relative dates and explicitly name rejected alternatives for decisions.
  entities: array of named entities mentioned (people, places, brands, products, named concepts)
  dates:    array of absolute dates referenced in the fact (YYYY-MM-DD format)

Example output for a session where the user joined "Book Lovers Unite" three weeks before the session_date and attended a meetup last week:
[
  {"type": "Experience", "text": "On 2023-05-03, the user joined the Facebook group 'Book Lovers Unite'.", "entities": ["Book Lovers Unite"], "dates": ["2023-05-03"]},
  {"type": "Experience", "text": "On 2023-05-17, the user attended a Book Lovers Unite meetup, two weeks after joining the group on 2023-05-03.", "entities": ["Book Lovers Unite"], "dates": ["2023-05-17", "2023-05-03"]}
]

Emit 2-`)
	fmt.Fprintf(&b, "%d", req.MaxFacts)
	b.WriteString(" facts. Be concise. No empty facts.\n")
	return b.String()
}

// parseExtractFactsResponse pulls the JSON array out of Tier 2's reply.
// Tolerates leading/trailing prose or code fences.
func parseExtractFactsResponse(raw string) ([]llmFactPayload, error) {
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found")
	}
	chunk := raw[start : end+1]
	var out []llmFactPayload
	if err := json.Unmarshal([]byte(chunk), &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

func truncForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
