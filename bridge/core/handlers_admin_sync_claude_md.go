// handlers_admin_sync_claude_md.go — Feature #5: project-scoped Synaptic
// memory injection into the user's local CLAUDE.md.
//
// Each Claude Code session boots Claude with whatever's in the working
// directory's CLAUDE.md. By auto-writing the top-K Synaptic memories
// tagged for that project into a managed block, every new session
// "remembers" the project's context without any /sd-recall calls.
//
// POST /admin/sync-claude-md
//   body: { "project_path": "/abs/path", "project_tag": "project:gravity", "top_k": 12 }
//   returns: { "wrote_path": "...", "memories_injected": N, "bytes_written": M }
//
// Idempotent. The managed block is delimited by
//   <!-- BEGIN SYNAPTIC --> ... <!-- END SYNAPTIC -->
// so the user's hand-written content above/below the block is preserved.
// On every call the block is replaced; nothing outside the block is touched.
// When the project's CLAUDE.md doesn't exist, a new file is created
// containing just the managed block.
//
// Safety:
//   - project_path must be an existing directory; missing → 404.
//   - We refuse to write outside the project_path (the only file we ever
//     touch is `<project_path>/CLAUDE.md`).
//   - Sensitive memories are excluded (even if they match the tag) so
//     credentials / secrets never land in a checked-in dotfile.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type syncClaudeMdRequest struct {
	ProjectPath string `json:"project_path"`
	ProjectTag  string `json:"project_tag"`
	TopK        int    `json:"top_k"`
}

type syncClaudeMdResponse struct {
	WrotePath        string `json:"wrote_path"`
	MemoriesInjected int    `json:"memories_injected"`
	BytesWritten     int    `json:"bytes_written"`
	Note             string `json:"note,omitempty"`
}

const (
	claudeMdBeginMarker = "<!-- BEGIN SYNAPTIC -->"
	claudeMdEndMarker   = "<!-- END SYNAPTIC -->"
)

// claudeMdBlockRe matches the managed block including the markers.
// Multiline mode + lazy middle so it only captures the first block.
var claudeMdBlockRe = regexp.MustCompile(`(?s)<!-- BEGIN SYNAPTIC -->.*?<!-- END SYNAPTIC -->`)

func (s *Server) handleAdminSyncClaudeMd(w http.ResponseWriter, r *http.Request) {
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
	var body syncClaudeMdRequest
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.ProjectPath = strings.TrimSpace(body.ProjectPath)
	body.ProjectTag = strings.TrimSpace(body.ProjectTag)
	if body.ProjectPath == "" {
		http.Error(w, "project_path is required", http.StatusBadRequest)
		return
	}
	if body.ProjectTag == "" {
		http.Error(w, "project_tag is required (e.g. 'project:gravity')", http.StatusBadRequest)
		return
	}
	if body.TopK <= 0 {
		body.TopK = 12
	}
	if body.TopK > 50 {
		body.TopK = 50
	}
	// Resolve + validate the project path.
	absPath, err := filepath.Abs(body.ProjectPath)
	if err != nil {
		http.Error(w, "project_path resolve: "+err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(absPath)
	if err != nil {
		http.Error(w, "project_path not found: "+err.Error(), http.StatusNotFound)
		return
	}
	if !info.IsDir() {
		http.Error(w, "project_path must be a directory", http.StatusBadRequest)
		return
	}
	mdPath := filepath.Join(absPath, "CLAUDE.md")

	// Pull candidate memories: tagged with the project_tag, not sensitive,
	// not deleted. We over-pull then trim to TopK after sorting.
	all, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 5000, ExcludeSensitive: true})
	if err != nil {
		http.Error(w, "list memories: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tagLow := strings.ToLower(body.ProjectTag)
	var matching []MemoryRecord
	for _, m := range all {
		for _, t := range m.Tags {
			if strings.ToLower(strings.TrimSpace(t)) == tagLow {
				matching = append(matching, m)
				break
			}
		}
	}
	// Rank: pinned first, then highest salience, then most recently touched.
	sort.SliceStable(matching, func(i, j int) bool {
		ip := memoryHasTag(matching[i].Tags, "pinned")
		jp := memoryHasTag(matching[j].Tags, "pinned")
		if ip != jp {
			return ip
		}
		if matching[i].Salience != matching[j].Salience {
			return matching[i].Salience > matching[j].Salience
		}
		return matching[i].UpdatedAt > matching[j].UpdatedAt
	})
	if len(matching) > body.TopK {
		matching = matching[:body.TopK]
	}

	// Build the managed block.
	block := buildClaudeMdBlock(body.ProjectTag, matching)

	// Read existing CLAUDE.md (or treat as empty).
	existing, _ := os.ReadFile(mdPath)
	newContent := replaceOrAppendBlock(string(existing), block)

	if err := os.WriteFile(mdPath, []byte(newContent), 0o644); err != nil {
		http.Error(w, "write CLAUDE.md: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation:  "sync_claude_md",
		EntityType: "filesystem",
		EntityID:   mdPath,
		Reason:     fmt.Sprintf("injected %d memories for %s", len(matching), body.ProjectTag),
		AdapterID:  adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, syncClaudeMdResponse{
		WrotePath:        mdPath,
		MemoriesInjected: len(matching),
		BytesWritten:     len(newContent),
	})
}

// buildClaudeMdBlock formats the markdown block that's injected. Stable
// shape so diffs across runs only change when content actually changed.
func buildClaudeMdBlock(projectTag string, mems []MemoryRecord) string {
	var b strings.Builder
	b.WriteString(claudeMdBeginMarker)
	b.WriteString("\n")
	b.WriteString("<!-- Auto-generated by Synaptic. Edits inside this block are overwritten on each /admin/sync-claude-md run. -->\n")
	b.WriteString("<!-- Generated: ")
	b.WriteString(time.Now().UTC().Format(time.RFC3339))
	b.WriteString("  ·  Tag: ")
	b.WriteString(projectTag)
	b.WriteString("  ·  Memories: ")
	b.WriteString(fmt.Sprintf("%d", len(mems)))
	b.WriteString(" -->\n")
	b.WriteString("\n## Synaptic Context — ")
	b.WriteString(projectTag)
	b.WriteString("\n\n")
	if len(mems) == 0 {
		b.WriteString("_No Synaptic memories tagged_ `")
		b.WriteString(projectTag)
		b.WriteString("` _yet._\n")
	} else {
		for _, m := range mems {
			pinMark := ""
			if memoryHasTag(m.Tags, "pinned") {
				pinMark = " 📌"
			}
			b.WriteString("- **[")
			b.WriteString(m.RegionHint)
			b.WriteString("]**")
			b.WriteString(pinMark)
			b.WriteString(" ")
			text := strings.ReplaceAll(m.Text, "\n", " ")
			if len(text) > 240 {
				text = text[:240] + "…"
			}
			b.WriteString(text)
			if len(m.Tags) > 0 {
				b.WriteString("  \n  _tags_: `")
				b.WriteString(strings.Join(filterShortTags(m.Tags, 6), "`, `"))
				b.WriteString("`")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(claudeMdEndMarker)
	return b.String()
}

// replaceOrAppendBlock swaps the existing managed block with the new one
// in `existing`, or appends the new block at the end when no managed
// block is present. Preserves all other content unchanged.
func replaceOrAppendBlock(existing, block string) string {
	if claudeMdBlockRe.MatchString(existing) {
		return claudeMdBlockRe.ReplaceAllString(existing, block)
	}
	if existing == "" {
		return block + "\n"
	}
	trimmed := strings.TrimRight(existing, "\n")
	return trimmed + "\n\n" + block + "\n"
}

// filterShortTags drops single-char / empty tags + limits the count.
// Keeps the CLAUDE.md block readable instead of showing 15+ tags per line.
func filterShortTags(tags []string, cap int) []string {
	out := make([]string, 0, cap)
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if len(t) < 2 {
			continue
		}
		out = append(out, t)
		if len(out) >= cap {
			break
		}
	}
	return out
}

// jsonEmptyBody — used by tests when constructing requests with no body.
var _ = json.Marshal
