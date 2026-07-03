// handlers_admin.go — privileged admin endpoints under /admin/.
//
// Currently:
//   POST /admin/env_write   — atomic-rewrite a host-side `.env` file with
//                             a single allow-listed key=value pair.
//   GET  /admin/env_check   — read-only sister of env_write: reports
//                             whether an allow-listed key is in `.env` and
//                             whether the running process has it loaded.
//                             Returns only key + booleans + length, NEVER
//                             the value.
//
// Why /admin and not /settings?
//
//   /admin namespace is for actions that mutate state OUTSIDE the bank
//   (host filesystem, container lifecycle, future restart hooks). They
//   are ALL audited with redacted payloads. /settings is for rows in
//   the bank's `settings` table and round-trips JSON values verbatim.
//
// Threat model for env_write:
//   - The endpoint is authed by SD_API_TOKEN like every other API call.
//   - Allow-list is hard-coded in code (not configurable from settings) to
//     stop a compromised dashboard from writing arbitrary host env vars.
//   - The pasted value never enters logs (we record only key + length).
//   - Audit trail records WHEN a key was rotated and HOW LONG the new
//     value is, never the value itself.
//   - Atomic rename means a partial write cannot truncate the existing
//     file; mid-write power loss leaves either the old or new content.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// envWriteAllowList caps which env-var names this endpoint will write.
// Expand here as new providers come online — DO NOT make this configurable
// from settings or query params.
var envWriteAllowList = map[string]bool{
	"SD_TIER1_API_KEY":          true,
	"SD_TIER2_API_KEY":          true,
	"SD_TIER3_API_KEY":          true,
	"SD_TIER1_API_OPENAI_KEY":   true, // iter 23: per-vendor split so a tier can
	"SD_TIER1_API_CLAUDE_KEY":   true, // hot-swap between OpenAI/Anthropic without
	"SD_TIER2_API_OPENAI_KEY":   true, // re-pasting the other vendor's key.
	"SD_TIER2_API_CLAUDE_KEY":   true,
	"SD_TIER3_API_OPENAI_KEY":   true,
	"SD_TIER3_API_CLAUDE_KEY":   true,
	"SD_NIGHTLY_API_KEY":        true, // forward-compat; no current consumer
	"SD_NIGHTLY_API_OPENAI_KEY": true,
	"SD_NIGHTLY_API_CLAUDE_KEY": true,
	"HUGGING_FACE_HUB_TOKEN":    true, // wave 8 — AirLLM gated-model auth
}

// envWriteMaxValueBytes is a sanity ceiling — real API keys are ~50–200 chars.
const envWriteMaxValueBytes = 4096

type envWriteRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) postAdminEnvWrite(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req envWriteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !envWriteAllowList[req.Key] {
		http.Error(w,
			fmt.Sprintf("key not on allow-list; allowed: %s", strings.Join(allowListNames(), ", ")),
			http.StatusBadRequest)
		return
	}
	if err := validateEnvValue(req.Value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	path := envFilePath()
	if err := writeEnvKeyAtomic(path, req.Key, req.Value); err != nil {
		// Permission error → 403 per handoff contract.
		if errors.Is(err, os.ErrPermission) || os.IsPermission(err) {
			http.Error(w, "env file not writable: "+err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "write env: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Audit log — REDACTED. Value never leaves this function.
	s.auditWrite(AuditEntry{
		Operation:  "env_write",
		EntityType: "env",
		EntityID:   req.Key,
		AfterJSON: fmt.Sprintf(
			`{"key":%q,"value_set":true,"value_len":%d,"path":%q}`,
			req.Key, len(req.Value), path,
		),
		AdapterID: adapterIDFromRequest(r),
	})

	// Log key + length only. Never the value.
	log.Printf("env_write: key=%s value_len=%d path=%s", req.Key, len(req.Value), path)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":  req.Key,
		"set":  true,
		"note": "Restart sd-core for the new key to take effect.",
	})
}

// allowListNames returns the sorted set of allow-listed key names for
// inclusion in error responses.
func allowListNames() []string {
	out := make([]string, 0, len(envWriteAllowList))
	for k := range envWriteAllowList {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envFilePath resolves where the .env file lives:
//   1. SD_ENV_FILE_PATH env var (explicit override; used by tests)
//   2. <SD_DATA_DIR>/.env (matches the unified compose: /data/.env in container)
func envFilePath() string {
	if p := os.Getenv("SD_ENV_FILE_PATH"); p != "" {
		return p
	}
	dataDir := envOr("SD_DATA_DIR", "/data")
	return filepath.Join(dataDir, ".env")
}

// validateEnvValue rejects values that would corrupt .env parsing or are
// otherwise inappropriate for an allow-listed API key.
//
// Allow-listed keys hold provider API tokens drawn from a constrained
// charset — base64-padded, plus `-` and `_` (e.g. `sk-proj-AbCd...=`).
// They never legitimately contain whitespace, `#`, or quote characters.
// Rejecting those classes here means the on-disk line never needs quoting,
// which sidesteps a whole family of dotenv-parser ambiguities.
func validateEnvValue(v string) error {
	if v == "" || strings.TrimSpace(v) == "" {
		return errors.New("value cannot be empty or whitespace-only")
	}
	if strings.ContainsRune(v, 0) {
		return errors.New("value cannot contain null bytes")
	}
	if strings.ContainsAny(v, " \t\n\r#\"") {
		return errors.New(`value contains characters not allowed for API keys (space, tab, newline, # or ")`)
	}
	if len(v) > envWriteMaxValueBytes {
		return fmt.Errorf("value exceeds %d-byte ceiling", envWriteMaxValueBytes)
	}
	return nil
}

// writeEnvKeyAtomic reads .env, replaces the line matching `<key>=` (if any)
// or appends a new line, then atomically renames a tmp file into place.
// Comments, blank lines, and ordering are preserved.
//
// Line endings: this function ALWAYS writes pure LF (`\n`) regardless of
// host OS. When reading the existing file we strip trailing `\r` from
// every line so a previously-CRLF-tainted file gets auto-healed on the
// next write. (Mixed terminators on Windows hosts caused the
// `Authorization: Bearer ...\r` 401 bug — see handoff 2026-05-09.)
//
// Quoting: this function does NOT quote the value. The validateEnvValue
// guard rejects every character class that would require quoting (space,
// tab, newline, `#`, `"`), so a bare `KEY=value` is unambiguous to every
// dotenv parser.
//
// Matching rule: a line is considered "the existing entry" when its
// leading-whitespace-stripped form starts with `<key>=`. This catches
// `KEY=value`, `  KEY=value`, `KEY= value` — but NOT `# KEY=value` (commented),
// `KEY_OTHER=...` (different key), or `KEY` alone (no `=`).
func writeEnvKeyAtomic(path, key, value string) error {
	var content []byte
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if data != nil {
		content = data
	}

	// Split + heal CRLF: strip trailing `\r` from every line. Anything
	// joined back with `\n` is now guaranteed pure-LF, and previously
	// damaged files normalise on the first write.
	rawLines := strings.Split(string(content), "\n")
	lines := make([]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	newLine := key + "=" + value

	found := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, key+"=") {
			lines[i] = newLine
			found = true
			break
		}
	}

	if !found {
		switch {
		case len(lines) == 1 && lines[0] == "":
			// Empty file (or strings.Split of "") → [""]. Replace with new line + trailing newline.
			lines = []string{newLine, ""}
		case len(lines) > 0 && lines[len(lines)-1] == "":
			// File ended with trailing newline; insert before it and re-add newline.
			lines[len(lines)-1] = newLine
			lines = append(lines, "")
		default:
			// File didn't end with newline; append + ensure trailing newline.
			lines = append(lines, newLine, "")
		}
	}
	output := strings.Join(lines, "\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(output)); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ──────────────────────────────────────────────────────────────────────────
// GET /admin/env_check?key=X
// ──────────────────────────────────────────────────────────────────────────

// envCheckResponse is the wire shape. value_length is the only quantitative
// data point — see the handoff's "value_length narrows brute-force search"
// reasoning for why this stays in the response (returned to authed dashboard
// only) but never in logs.
type envCheckResponse struct {
	Key          string `json:"key"`
	InEnvFile    bool   `json:"in_env_file"`
	ValueLength  int    `json:"value_length"`
	InProcessEnv bool   `json:"in_process_env"`
}

func (s *Server) getAdminEnvCheck(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "key query param is required", http.StatusBadRequest)
		return
	}
	if !envWriteAllowList[key] {
		http.Error(w,
			fmt.Sprintf("key not on allow-list; allowed: %s", strings.Join(allowListNames(), ", ")),
			http.StatusBadRequest)
		return
	}

	path := envFilePath()
	inFile, valueLen, err := readEnvFileKey(path, key)
	if err != nil {
		// Permission error → 403 per handoff contract. File-missing already
		// resolved inside readEnvFileKey to (false, 0, nil).
		if errors.Is(err, os.ErrPermission) || os.IsPermission(err) {
			http.Error(w, "env file not readable: "+err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "read env: "+err.Error(), http.StatusInternalServerError)
		return
	}

	inProcess := os.Getenv(key) != ""

	// Log key NAME and BOOLEANS only. The handoff explicitly says NOT to log
	// value_length — even a length is a brute-force search-narrowing leak.
	log.Printf("env_check: key=%s in_env_file=%v in_process=%v",
		key, inFile, inProcess)

	writeJSON(w, http.StatusOK, envCheckResponse{
		Key:          key,
		InEnvFile:    inFile,
		ValueLength:  valueLen,
		InProcessEnv: inProcess,
	})
}

// readEnvFileKey scans the .env file for a strict line-start match of
// `<key>=`. Returns (in_file, value_length_after_unquoting, error).
//
// File-missing is NOT an error: returns (false, 0, nil). Permission errors
// (read denied) ARE returned so the caller can map to 403.
//
// Match rules per the handoff:
//   - Case-sensitive
//   - Line-start (NO leading whitespace tolerated — "ignore indented lines")
//   - Comment lines (starting with `#`) are skipped
//   - When duplicate keys exist, the LAST match wins (matches dotenv-style
//     parser behaviour where later assignments shadow earlier ones)
//   - Outer double-quotes or single-quotes are stripped before length count,
//     so `KEY="abc"` reports length 3, not 5. Escape sequences inside the
//     quotes are NOT interpreted — we report raw character count, which is
//     what the user typed.
func readEnvFileKey(path, key string) (bool, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}

	prefix := key + "="
	var lastValue string
	found := false
	for _, raw := range strings.Split(string(data), "\n") {
		// Heal CRLF: strip trailing `\r` so a CRLF-tainted file reports the
		// correct value_length and matches `KEY=value` cleanly. (Same root-
		// cause as the env_write CRLF bug.)
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		// Strict line-start match — handoff: "Match is case-sensitive and at
		// line start (ignore comments, indented lines)". Do NOT trim leading
		// whitespace before the prefix check.
		if strings.HasPrefix(line, prefix) {
			lastValue = strings.TrimPrefix(line, prefix)
			found = true
			// Continue scanning — duplicates resolve to the LAST occurrence.
		}
	}

	if !found {
		return false, 0, nil
	}
	return true, len(unquoteEnvValue(lastValue)), nil
}

// unquoteEnvValue strips a single layer of matched outer quotes. We don't
// process escape sequences — the goal is character count for user feedback,
// not value reconstruction. Mismatched quotes (only one side) leave the
// string untouched.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
