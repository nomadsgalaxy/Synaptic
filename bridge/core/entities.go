// entities.go — v2.7 Bundle N: canonical-name + alias graph.
//
// A small registry that maps surface aliases ("AC", "the boss") onto
// canonical names ("Anthropic", "Sarah Chen"). Recall expansion in
// recall.go reads this table to OR the canonical name into the BM25
// query when the user types an alias.
//
// Scope is deliberately minimal: no embedding store on entities, no
// auto-cluster, no co-reference resolution. Phase 6 of the nightly
// pipeline can populate the table later via LLM extraction; for now
// callers seed it through the bank methods (and a future MCP tool).
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Entity is one canonical-name row. ID is opaque (caller-supplied or
// auto-generated like other bank IDs); CanonicalName is unique modulo
// case (UNIQUE INDEX with COLLATE NOCASE).
type Entity struct {
	ID            string `json:"id"`
	CanonicalName string `json:"canonical_name"`
	Kind          string `json:"kind,omitempty"` // person|org|product|place|... (free-form)
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// EntityAlias is one (entity_id, alias) edge. Source records who
// claimed the mapping (manual / phase6 / mcp) so a future cleanup
// pass can prune low-confidence aliases.
type EntityAlias struct {
	EntityID  string `json:"entity_id"`
	Alias     string `json:"alias"`
	Source    string `json:"source,omitempty"`
	CreatedAt string `json:"created_at"`
}

// UpsertEntity creates or updates an entity. ID is generated if empty;
// the row is upserted on canonical_name (case-insensitive). Returns the
// resulting record so callers learn the assigned ID.
func (b *Bank) UpsertEntity(e Entity) (Entity, error) {
	if b == nil {
		return Entity{}, errors.New("bank not enabled")
	}
	if strings.TrimSpace(e.CanonicalName) == "" {
		return Entity{}, errors.New("canonical_name is required")
	}
	now := nowUTC()
	if e.CreatedAt == "" {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	if e.ID == "" {
		e.ID = newID("entity")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO entities (id, canonical_name, kind, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(canonical_name) DO UPDATE SET
    kind       = excluded.kind,
    updated_at = excluded.updated_at`,
		e.ID, e.CanonicalName, e.Kind, e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return Entity{}, fmt.Errorf("upsert entity: %w", err)
	}
	// On conflict the existing row's ID wins — read it back so the caller
	// gets a coherent record.
	var got Entity
	row := b.db.QueryRow(
		`SELECT id, canonical_name, kind, created_at, updated_at
		 FROM entities WHERE canonical_name = ? COLLATE NOCASE`,
		e.CanonicalName)
	if err := row.Scan(&got.ID, &got.CanonicalName, &got.Kind, &got.CreatedAt, &got.UpdatedAt); err != nil {
		return e, err
	}
	return got, nil
}

// GetEntity returns the entity row by ID, or sql.ErrNoRows.
func (b *Bank) GetEntity(id string) (Entity, error) {
	if b == nil {
		return Entity{}, errors.New("bank not enabled")
	}
	row := b.db.QueryRow(
		`SELECT id, canonical_name, kind, created_at, updated_at
		 FROM entities WHERE id = ?`, id)
	var e Entity
	if err := row.Scan(&e.ID, &e.CanonicalName, &e.Kind, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return Entity{}, err
	}
	return e, nil
}

// ListEntities returns up to limit rows, ordered by canonical_name.
func (b *Bank) ListEntities(limit int) ([]Entity, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := b.db.Query(
		`SELECT id, canonical_name, kind, created_at, updated_at
		 FROM entities ORDER BY canonical_name COLLATE NOCASE LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entity{}
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.CanonicalName, &e.Kind, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEntity removes an entity and (via ON DELETE CASCADE) all its
// aliases. Returns sql.ErrNoRows on missing id.
func (b *Bank) DeleteEntity(id string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM entities WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AddAlias inserts (entity_id, alias) — idempotent on conflict. alias
// is trimmed; empty alias errors. Mixed-case aliases are preserved as
// stored (lookup matches case-insensitively).
func (b *Bank) AddAlias(entityID, alias, source string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return errors.New("alias cannot be empty")
	}
	if source == "" {
		source = "manual"
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`INSERT OR IGNORE INTO entity_aliases (entity_id, alias, source, created_at)
		 VALUES (?, ?, ?, ?)`,
		entityID, alias, source, nowUTC())
	return err
}

// RemoveAlias is the inverse.
func (b *Bank) RemoveAlias(entityID, alias string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`DELETE FROM entity_aliases WHERE entity_id = ? AND alias = ? COLLATE NOCASE`,
		entityID, alias)
	return err
}

// ListAliases returns all aliases for an entity.
func (b *Bank) ListAliases(entityID string) ([]EntityAlias, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(
		`SELECT entity_id, alias, source, created_at
		 FROM entity_aliases WHERE entity_id = ? ORDER BY alias COLLATE NOCASE`,
		entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EntityAlias{}
	for rows.Next() {
		var a EntityAlias
		if err := rows.Scan(&a.EntityID, &a.Alias, &a.Source, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveAlias takes a surface token and returns the canonical name(s)
// it could refer to. Case-insensitive. Empty input → empty result.
// Multiple entities can share an alias (e.g., "Apple" → company AND
// fruit); the caller decides whether to expand to all matches.
//
// Used by /recall query expansion: if any token in the query matches
// an alias, the canonical name is OR'd into the BM25 search.
func (b *Bank) ResolveAlias(alias string) ([]string, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, nil
	}
	rows, err := b.db.Query(`
		SELECT e.canonical_name
		FROM entity_aliases a JOIN entities e ON e.id = a.entity_id
		WHERE a.alias = ? COLLATE NOCASE`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ExpandQueryViaAliases scans the query for tokens that are known aliases
// and returns the canonical names to include in a downstream BM25 search.
// Returns nil when nothing expands — recall.go's caller short-circuits
// in that case and uses the raw query alone. Tokenisation is lightweight
// (whitespace + simple punctuation); multi-word aliases ("the boss") are
// matched as bigrams/trigrams up to 4 tokens.
func (b *Bank) ExpandQueryViaAliases(query string) ([]string, error) {
	if b == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	tokens := tokenizeForAliasMatch(query)
	if len(tokens) == 0 {
		return nil, nil
	}
	// Generate candidate phrases: unigrams, bigrams, trigrams, 4-grams.
	candidates := []string{}
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[strings.ToLower(p)] {
			return
		}
		seen[strings.ToLower(p)] = true
		candidates = append(candidates, p)
	}
	for n := 1; n <= 4; n++ {
		for i := 0; i+n <= len(tokens); i++ {
			add(strings.Join(tokens[i:i+n], " "))
		}
	}
	// Resolve each candidate. Stop early on absurdly long candidate
	// lists (very long queries) — capped at 200 lookups.
	if len(candidates) > 200 {
		candidates = candidates[:200]
	}
	out := []string{}
	dedup := map[string]bool{}
	for _, c := range candidates {
		names, err := b.ResolveAlias(c)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !dedup[strings.ToLower(n)] {
				dedup[strings.ToLower(n)] = true
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// tokenizeForAliasMatch splits a query into alias-candidate tokens.
// Replaces non-alphanumeric chars with spaces; collapses whitespace.
// Case is preserved (ResolveAlias is case-insensitive).
func tokenizeForAliasMatch(query string) []string {
	var b strings.Builder
	for _, r := range query {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '\'':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}
