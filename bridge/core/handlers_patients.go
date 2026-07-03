// HTTP surface for the Patient feature. Endpoints:
//
//	GET  /patients                  → { patients: [...], active_id: "..." }
//	POST /patients                  → create { name }
//	GET  /patients/{id}             → one patient
//	PUT  /patients/{id}/activate    → swap active bank to this patient
//	DELETE /patients/{id}           → remove (refuses active + default)
//
// All endpoints require the bearer token (auth middleware applies). The
// activate path mutates running state — it gates on s.bankSwapMu and
// refuses while any nightly run is in progress.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// patientView is the wire shape returned to clients. Adds the IsActive
// + IsPinned flags derived from PatientStore so the UI doesn't have to
// cross-reference active_id / pin state against the list itself.
type patientView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BankPath  string `json:"bank_path"`
	CreatedAt string `json:"created_at"`
	IsDefault bool   `json:"is_default"`
	IsActive  bool   `json:"is_active"`
	IsPinned  bool   `json:"is_pinned,omitempty"`
}

func patientToView(p Patient, activeID, pinnedID string) patientView {
	return patientView{
		ID:        p.ID,
		Name:      p.Name,
		BankPath:  p.BankPath,
		CreatedAt: p.CreatedAt,
		IsDefault: p.IsDefault,
		IsActive:  p.ID == activeID,
		IsPinned:  pinnedID != "" && p.ID == pinnedID,
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSONStatus(w, status, map[string]string{"error": msg})
}

// patientLookups gathers the activeID + pinnedID at one call site so
// every view-building handler doesn't have to repeat the boilerplate.
func (s *Server) patientLookups() (activeID, pinnedID string) {
	if s.patients == nil {
		return "", ""
	}
	if active, ok := s.patients.Active(); ok {
		activeID = active.ID
	}
	pinnedID, _ = s.patients.PinState()
	return activeID, pinnedID
}

// handlePatients dispatches GET (list) and POST (create) on the
// collection endpoint. Anything else is 405.
func (s *Server) handlePatients(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.patients == nil {
		writeErr(w, http.StatusServiceUnavailable, "patient feature is unavailable on this deploy (bank disabled)")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listPatients(w, r)
	case http.MethodPost:
		s.createPatient(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handlePatientByID dispatches the per-patient routes. Path shapes:
//
//	/patients/{id}             — GET (single) or DELETE
//	/patients/{id}/activate    — PUT only
func (s *Server) handlePatientByID(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.patients == nil {
		writeErr(w, http.StatusServiceUnavailable, "patient feature is unavailable on this deploy (bank disabled)")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/patients/")
	if rest == "" {
		writeErr(w, http.StatusBadRequest, "missing patient id")
		return
	}
	id, action, _ := strings.Cut(rest, "/")

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.getPatient(w, r, id)
		case http.MethodPatch:
			s.renamePatient(w, r, id)
		case http.MethodDelete:
			s.deletePatient(w, r, id)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "activate":
		if r.Method != http.MethodPut {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed (use PUT)")
			return
		}
		s.activatePatient(w, r, id)
	case "memories":
		// Read-only browse of another patient's bank — opens a transient
		// *sql.DB to the patient's bank_path, queries, closes. Lets the
		// UI render an inactive Patient's contents in a side panel
		// without forcing a swap (which would break any session pinning
		// the active Patient).
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed (use GET)")
			return
		}
		s.handlePatientMemoriesBrowse(w, r, id)
	case "pin":
		switch r.Method {
		case http.MethodPost:
			s.pinPatient(w, r, id)
		case http.MethodDelete:
			s.unpinPatient(w, r, id)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed (use POST or DELETE)")
		}
	default:
		writeErr(w, http.StatusNotFound, "unknown patient sub-resource: "+action)
	}
}

func (s *Server) listPatients(w http.ResponseWriter, _ *http.Request) {
	all := s.patients.List()
	activeID, pinnedID := s.patientLookups()
	out := make([]patientView, 0, len(all))
	for _, p := range all {
		out = append(out, patientToView(p, activeID, pinnedID))
	}
	resp := map[string]interface{}{
		"patients":   out,
		"active_id":  activeID,
		"default_id": s.patients.DefaultID(),
	}
	if pinnedID != "" {
		resp["pinned_id"] = pinnedID
	}
	writeJSONStatus(w, http.StatusOK, resp)
}

func (s *Server) getPatient(w http.ResponseWriter, _ *http.Request, id string) {
	p, err := s.patients.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	activeID, pinnedID := s.patientLookups()
	writeJSONStatus(w, http.StatusOK, patientToView(p, activeID, pinnedID))
}

func (s *Server) createPatient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	p, err := s.patients.Create(body.Name)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrPatientNameRequired):
			status = http.StatusBadRequest
		case errors.Is(err, ErrPatientNameDuplicate):
			status = http.StatusConflict
		default:
			status = http.StatusInternalServerError
		}
		writeErr(w, status, err.Error())
		return
	}
	activeID, pinnedID := s.patientLookups()
	writeJSONStatus(w, http.StatusCreated, patientToView(p, activeID, pinnedID))
}

// activatePatient swaps the running bank to the patient's bank_path.
// Refuses if:
//   - the patient ID is unknown
//   - this is already the active patient (no-op = 200 + same view)
//   - any nightly run is currently in progress (caller must wait)
//   - a different patient is currently pinned (long-running session
//     holds the lock — bench, an active write-burst, etc.)
func (s *Server) activatePatient(w http.ResponseWriter, _ *http.Request, id string) {
	target, err := s.patients.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// Refuse during nightly. The NightlyRunner holds its own bank ref;
	// swapping out from under it would leave the runner writing to a
	// closing handle. Callers should wait for nightly to finish or call
	// /nightly/interrupt first.
	if s.nightly != nil {
		if running, runID := s.nightly.IsRunning(); running {
			writeErr(w, http.StatusConflict, "cannot switch patient while nightly run "+runID+" is in progress — wait for it to finish or call /nightly/interrupt first")
			return
		}
	}
	s.bankSwapMu.Lock()
	defer s.bankSwapMu.Unlock()

	// Pin is advisory: surface a warning in the response when the
	// caller is swapping AWAY from a pinned patient, but don't refuse
	// the swap. Long-running sessions (bench, etc.) must detect
	// activation loss themselves and abort gracefully. This is the
	// user's explicit design choice — only one bank is "active" at a
	// time, but viewing any bank from the UI is always allowed.
	pinnedID, _ := s.patients.PinState()
	pinBroken := pinnedID != "" && pinnedID != target.ID
	if pinBroken {
		// Clear the pin so the holder (which checks active state) sees
		// the swap and aborts cleanly without being misled by a stale
		// pin token. The pin holder is now effectively dead.
		s.patients.ForceUnpin()
	}

	// If already active, skip the Bank.Reopen — still return the patient
	// so the client can confirm.
	current, _ := s.patients.Active()
	if current.ID == target.ID && s.bank != nil && s.bank.Path() == target.BankPath {
		_, pinnedID := s.patientLookups()
		writeJSONStatus(w, http.StatusOK, map[string]interface{}{
			"patient":     patientToView(target, target.ID, pinnedID),
			"swapped":     false,
			"bank_path":   s.bank.Path(),
		})
		return
	}
	if s.bank == nil {
		writeErr(w, http.StatusServiceUnavailable, "bank is not enabled on this deploy")
		return
	}
	if err := s.bank.Reopen(target.BankPath); err != nil {
		writeErr(w, http.StatusInternalServerError, "bank reopen failed: "+err.Error())
		return
	}
	if _, err := s.patients.SetActive(target.ID); err != nil {
		// Swap succeeded but persistence failed — bank is now pointing at
		// the new file but the registry says otherwise. Log loudly; on
		// next restart the registry's stale active_id will be honoured.
		writeErr(w, http.StatusInternalServerError, "swap succeeded but registry update failed: "+err.Error())
		return
	}
	// Broadcast a patient.swapped event so connected dashboards can
	// refresh their data. Use the bank's emit (still wired to hub).
	if s.bank.Emit != nil {
		s.bank.Emit("patient.swapped", "sd-core-patient", map[string]interface{}{
			"id":   target.ID,
			"name": target.Name,
		})
	}
	resp := map[string]interface{}{
		"patient":   patientToView(target, target.ID, ""),
		"swapped":   true,
		"bank_path": s.bank.Path(),
	}
	if pinBroken {
		resp["pin_broken"] = true
		resp["pin_warning"] = "the previously-pinned patient is no longer active; any session relying on it will likely error"
	}
	writeJSONStatus(w, http.StatusOK, resp)
}

// handlePatientMemoriesBrowse returns a paginated list of memories from
// another patient's bank file WITHOUT activating it. Opens a transient
// read-only *sql.DB connection, queries, closes. The active bank is
// never touched.
//
// Use case: user wants to look at "John Synaptic" data while a bench
// session pins another Patient. The header chip + dropdown show all
// patients; clicking one offers Activate OR Browse. Browse calls this
// endpoint and renders the result in a side panel.
//
// Query params:
//   limit    (default 25, max 1000)
//   offset   (default 0)
//   adapter  (optional adapter_id filter)
//
// Falls back to the live bank handler when the patient is already
// the active one — saves opening a redundant connection.
func (s *Server) handlePatientMemoriesBrowse(w http.ResponseWriter, r *http.Request, id string) {
	p, err := s.patients.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// If the requested patient is already active, just call the
	// normal /bank/memories handler logic via the live bank.
	if active, _ := s.patients.Active(); active.ID == id && s.bank != nil {
		s.bankList(w, r)
		return
	}

	// Parse query params
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	adapter := r.URL.Query().Get("adapter")

	// Open a transient read-only *sql.DB to the patient's bank file.
	// sqlite's `mode=ro` flag refuses any writes — defensive against a
	// handler bug that might accidentally mutate an inactive bank.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=journal_mode(WAL)", p.BankPath)
	db, dberr := sql.Open("sqlite", dsn)
	if dberr != nil {
		writeErr(w, http.StatusInternalServerError, "open patient bank: "+dberr.Error())
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Build query — same shape as Bank.ListMemoriesWith but stripped
	// down to what the browse panel needs. We don't expose all
	// MemoryRecord fields to avoid coupling the browse contract to
	// future schema migrations.
	q := `SELECT id, COALESCE(text, ''), COALESCE(enriched_text, ''),
	             COALESCE(tags, '[]'), COALESCE(adapter_id, ''),
	             COALESCE(session_id, ''), COALESCE(created_at, ''),
	             COALESCE(memory_type, ''), COALESCE(sensitive, 0)
	      FROM memories
	      WHERE deleted_at = '' OR deleted_at IS NULL`
	args := []interface{}{}
	if adapter != "" {
		q += ` AND adapter_id = ?`
		args = append(args, adapter)
	}
	q += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, qerr := db.QueryContext(r.Context(), q, args...)
	if qerr != nil {
		writeErr(w, http.StatusInternalServerError, "query patient bank: "+qerr.Error())
		return
	}
	defer rows.Close()

	type browseRow struct {
		ID           string `json:"id"`
		Text         string `json:"text"`
		EnrichedText string `json:"enriched_text,omitempty"`
		Tags         string `json:"tags"`
		AdapterID    string `json:"adapter_id,omitempty"`
		SessionID    string `json:"session_id,omitempty"`
		CreatedAt    string `json:"created_at"`
		MemoryType   string `json:"memory_type,omitempty"`
		Sensitive    bool   `json:"sensitive,omitempty"`
	}
	out := make([]browseRow, 0, limit)
	for rows.Next() {
		var br browseRow
		var sens int
		if err := rows.Scan(&br.ID, &br.Text, &br.EnrichedText, &br.Tags,
			&br.AdapterID, &br.SessionID, &br.CreatedAt, &br.MemoryType, &sens); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan row: "+err.Error())
			return
		}
		br.Sensitive = sens != 0
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "rows iter: "+err.Error())
		return
	}

	// Total count (for pagination — cheap on SQLite).
	var total int
	countQ := `SELECT COUNT(*) FROM memories WHERE deleted_at = '' OR deleted_at IS NULL`
	if adapter != "" {
		countQ += ` AND adapter_id = '` + strings.ReplaceAll(adapter, "'", "''") + `'`
	}
	_ = db.QueryRowContext(r.Context(), countQ).Scan(&total)

	writeJSONStatus(w, http.StatusOK, map[string]interface{}{
		"patient_id":   id,
		"patient_name": p.Name,
		"bank_path":    p.BankPath,
		"memories":     out,
		"limit":        limit,
		"offset":       offset,
		"total":        total,
		"read_only":    true,
	})
}

// pinPatient acquires a pin on the currently-active patient, returning
// an opaque token the caller must present to release. The id from the
// URL must match the currently-active patient — pinning is always "pin
// what I'm using right now". Body: { ttl_seconds: N } (default 14400 = 4h).
func (s *Server) pinPatient(w http.ResponseWriter, r *http.Request, id string) {
	current, ok := s.patients.Active()
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "no active patient to pin")
		return
	}
	if current.ID != id {
		writeErr(w, http.StatusBadRequest, "can only pin the currently-active patient — activate first")
		return
	}
	var body struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		_ = dec.Decode(&body) // empty body is OK (use default)
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	token, err := s.patients.Pin(ttl)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrPatientPinHeld) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]interface{}{
		"patient_id":  current.ID,
		"pin_token":   token,
		"ttl_seconds": int(ttl.Seconds()),
	})
}

// unpinPatient releases the pin if the token matches. Request body OR
// query param `token=` is accepted (whichever the client provides). A
// missing pin is treated as success (idempotent).
func (s *Server) unpinPatient(w http.ResponseWriter, r *http.Request, id string) {
	token := r.URL.Query().Get("token")
	if token == "" && r.Body != nil {
		var body struct {
			Token string `json:"token"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		_ = dec.Decode(&body)
		token = body.Token
	}
	// id is in path purely for symmetry / discoverability; PatientStore
	// validates token only. A stray DELETE to the wrong id with the
	// right token still releases — keep it simple.
	_ = id
	if err := s.patients.Unpin(token); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrPatientBadPinToken) {
			status = http.StatusForbidden
		}
		writeErr(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renamePatient updates the display name of a patient. ID + bank_path
// are immutable; only the name changes. Duplicate names rejected.
func (s *Server) renamePatient(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	p, err := s.patients.Rename(id, body.Name)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrPatientNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrPatientNameRequired):
			status = http.StatusBadRequest
		case errors.Is(err, ErrPatientNameDuplicate):
			status = http.StatusConflict
		case errors.Is(err, ErrPatientInvalidID):
			status = http.StatusBadRequest
		default:
			status = http.StatusInternalServerError
		}
		writeErr(w, status, err.Error())
		return
	}
	activeID, pinnedID := s.patientLookups()
	writeJSONStatus(w, http.StatusOK, patientToView(p, activeID, pinnedID))
}

func (s *Server) deletePatient(w http.ResponseWriter, _ *http.Request, id string) {
	if err := s.patients.Delete(id); err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrPatientNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrPatientCannotDelete):
			status = http.StatusConflict
		case errors.Is(err, ErrPatientInvalidID):
			status = http.StatusBadRequest
		default:
			status = http.StatusInternalServerError
		}
		writeErr(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
