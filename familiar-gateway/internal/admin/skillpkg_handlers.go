package admin

// Imported-skills console API (SKILL-PACKAGES-SPEC Phase 2).
// Library management (import / rescan / enable / disable / delete)
// is ADMIN-ONLY — code-and-prompt admission is an operator decision.
// Listing is any-authed-user (owners need the catalog to bind), and
// binding packages to a shard is owner-scoped like every other shard
// mutation. Shard sessions are refused throughout.
//
// The import flow is approve-on-import (spec decision): the same
// endpoint runs as a dry-run preview by default and only admits the
// package when confirm=true — the admin sees name, description,
// provenance, script/wasm flags, and the allowed-tools mapping
// before anything lands in the library.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/skillpkg"
	"github.com/familiar/gateway/internal/skills/fetch"
)

// AttachSkillPackages wires the imported-skills store plus the
// known-tool set used for allowed-tools mapping at import time.
func (h *Handler) AttachSkillPackages(store *skillpkg.Store, knownTools map[string]bool) {
	h.skillPkgs = store
	h.skillPkgKnownTools = knownTools
}

func (h *Handler) requireSkillPkgs(w http.ResponseWriter) bool {
	if h.skillPkgs == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "imported skills not configured")
		return false
	}
	return true
}

func (h *Handler) listSkillPackages(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	if h.refuseShardSession(w, r) {
		return
	}
	items, err := h.skillPkgs.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Request body caps. Nothing bounded these bodies: a multipart upload
// spilled to temp disk for as long as it streamed, and a JSON body was
// decoded whole into memory, from any signed-in user.
const (
	maxImportRequestBytes = 22 << 20  // a 21MB archive plus form overhead
	maxSkillRequestBytes  = 512 << 10 // authored skills (256KB body) and small JSON
)

// limitBody caps r's body at n bytes; reading past it fails.
func limitBody(w http.ResponseWriter, r *http.Request, n int64) {
	r.Body = http.MaxBytesReader(w, r.Body, n)
}

// importPayload is an import request: the archive, whether it's the
// confirm step, the source URL, and the digest the preview showed.
type importPayload struct {
	data      []byte
	confirm   bool
	sourceURL string
	digest    string
}

// readImportPayload extracts an import request: multipart/form-data
// with a "file" zip part (and optional "confirm" and "digest" fields),
// or JSON {"url": ..., "confirm": bool, "digest": ...} for a direct-zip
// fetch.
func readImportPayload(w http.ResponseWriter, r *http.Request) (importPayload, error) {
	limitBody(w, r, maxImportRequestBytes)
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(25 << 20); err != nil {
			return importPayload{}, fmt.Errorf("invalid upload: %w", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			return importPayload{}, fmt.Errorf("missing file part")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 21<<20))
		if err != nil {
			return importPayload{}, fmt.Errorf("read upload: %w", err)
		}
		return importPayload{data: data, confirm: r.FormValue("confirm") == "true", digest: r.FormValue("digest")}, nil
	default:
		var body struct {
			URL     string `json:"url"`
			Confirm bool   `json:"confirm"`
			Digest  string `json:"digest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return importPayload{}, fmt.Errorf("invalid body: %w", err)
		}
		if body.URL == "" {
			return importPayload{}, fmt.Errorf("url required (or upload a zip via multipart)")
		}
		fetched, err := fetchSkillArchive(r.Context(), body.URL)
		if err != nil {
			return importPayload{}, err
		}
		return importPayload{data: fetched, confirm: body.Confirm, sourceURL: body.URL, digest: body.Digest}, nil
	}
}

func (h *Handler) writeImportPreview(w http.ResponseWriter, data []byte) {
	loaded, matched, unmatched, err := h.skillPkgs.PreviewZip(data, h.skillPkgKnownTools)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"preview":          true,
		"frontmatter":      loaded.Frontmatter,
		"digest":           loaded.Digest,
		"has_scripts":      loaded.HasScripts,
		"has_wasm":         loaded.HasWasm,
		"signature_status": "unsigned",
		"tools_matched":    matched,
		"tools_unmatched":  unmatched,
		"body_preview":     truncateString(loaded.Body, 2000),
	})
}

// importSkillPackage serves POST /console/api/skillpacks/import
// (admin-only, instance library). confirm=false (default) returns the
// preview only.
func (h *Handler) importSkillPackage(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	in, err := readImportPayload(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !in.confirm {
		h.writeImportPreview(w, in.data)
		return
	}
	// Admit only what the admin previewed. Confirm re-uploads or
	// re-fetches the archive, and a URL (or anything on the path of a
	// plain http one) could serve a different package the second time.
	loaded, _, _, err := h.skillPkgs.PreviewZip(in.data, h.skillPkgKnownTools)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.digest == "" || loaded.Digest != in.digest {
		writeJSONError(w, http.StatusConflict, "the package isn't the one previewed (its digest changed) — preview it again")
		return
	}
	pkg, err := h.skillPkgs.ImportZip(r.Context(), in.data, au.UserID, in.sourceURL, h.skillPkgKnownTools)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, pkg)
}

func (h *Handler) rescanSkillPackages(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	added, updated, missing, errs := h.skillPkgs.Rescan(r.Context(), au.UserID, h.skillPkgKnownTools)
	writeJSON(w, http.StatusOK, map[string]any{
		"added": added, "updated": updated, "missing": missing, "errors": errs,
	})
}

// requireInstancePkg resolves {id} and refuses user-owned rows: the
// admin /skillpacks routes manage the INSTANCE library only. User
// skills are their owner's to manage via /skills/mine.
func (h *Handler) requireInstancePkg(w http.ResponseWriter, r *http.Request) (*skillpkg.Package, bool) {
	p, err := h.skillPkgs.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, skillpkg.ErrNotFound) || (err == nil && p.OwnerID != "") {
		writeJSONError(w, http.StatusNotFound, "skill package not found")
		return nil, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return p, true
}

func (h *Handler) setSkillPackageDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.requireSkillPkgs(w) {
			return
		}
		p, ok := h.requireInstancePkg(w, r)
		if !ok {
			return
		}
		if err := h.skillPkgs.SetDisabled(r.Context(), p.ID, disabled); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p, err := h.skillPkgs.Get(r.Context(), p.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) deleteSkillPackage(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	p, ok := h.requireInstancePkg(w, r)
	if !ok {
		return
	}
	if err := h.skillPkgs.Delete(r.Context(), p.ID); err != nil {
		// Builtins refuse deletion by design (they re-sync at boot);
		// that's a caller mistake, not a server fault.
		status := http.StatusInternalServerError
		if errors.Is(err, skillpkg.ErrBuiltinImmutable) {
			status = http.StatusBadRequest
		}
		writeJSONError(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setSkillPackageChat serves POST /console/api/skillpacks/{id}/chat
// (admin-only): flips an INSTANCE package's trusted-chat exposure. In
// practice this is the builtin off switch — only origin='builtin' rows
// are reachable on the trusted path, so the toggle lets an admin pull
// a shipped skill out of every user's chat prompt (and put it back)
// without deleting anything. Mirrors setMySkillChat's contract:
// {"enabled": bool} in, the refreshed package out.
func (h *Handler) setSkillPackageChat(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	limitBody(w, r, maxSkillRequestBytes)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	p, ok := h.requireInstancePkg(w, r)
	if !ok {
		return
	}
	// Only builtins are trusted-chat eligible at instance scope
	// (USER-SKILLS-SPEC §3.2: imported/authored instance skills stay
	// shard-only). The store's serve-path predicate already enforces
	// this; refusing here keeps the flag from lying in the catalog.
	if p.Origin != "builtin" {
		writeJSONError(w, http.StatusBadRequest, "only built-in skills can be chat-enabled instance-wide")
		return
	}
	if err := h.skillPkgs.SetChatEnabled(r.Context(), p.ID, body.Enabled); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, err := h.skillPkgs.Get(r.Context(), p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── User skills (/console/api/skills/mine — USER-SKILLS-SPEC Phase A) ──
//
// Owner-scoped, any authed USER session (shard sessions refused): a
// user manages their own private library without an admin. Imports
// land under <skills.dir>/users/<uid>/ and are usable only by the
// owner's shards (Phase A; trusted-path exposure is Phase B).

// requireUserSession returns the authed non-shard user or writes the
// refusal.
func (h *Handler) requireUserSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.refuseShardSession(w, r) {
		return "", false
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return "", false
	}
	return au.UserID, true
}

func (h *Handler) listMySkills(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	items, err := h.skillPkgs.ListForOwner(r.Context(), uid)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) setMySkillDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.requireSkillPkgs(w) {
			return
		}
		uid, ok := h.requireUserSession(w, r)
		if !ok {
			return
		}
		p, err := h.skillPkgs.GetOwned(r.Context(), uid, r.PathValue("id"))
		if errors.Is(err, skillpkg.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "skill not found")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := h.skillPkgs.SetDisabled(r.Context(), p.ID, disabled); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p, err = h.skillPkgs.GetOwned(r.Context(), uid, p.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

// setMySkillChat flips the trusted-path opt-in (USER-SKILLS-SPEC
// Phase B): with chat_enabled the skill's name+description enter the
// owner's chat system prompt and use_skill can fetch its body there.
// The provenance warning for imported skills lives in the UI; the API
// records the owner's decision either way.
func (h *Handler) setMySkillChat(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	limitBody(w, r, maxSkillRequestBytes)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	p, err := h.skillPkgs.GetOwned(r.Context(), uid, r.PathValue("id"))
	if errors.Is(err, skillpkg.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "skill not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.skillPkgs.SetChatEnabled(r.Context(), p.ID, body.Enabled); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, err = h.skillPkgs.GetOwned(r.Context(), uid, p.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── Authoring (USER-SKILLS-SPEC Phase C) ─────────────────────────

// putMySkill serves PUT /console/api/skills/mine/{name}: create or
// update an authored skill from {description, body}. The server
// composes the SKILL.md — the editor never writes raw frontmatter.
// Imported skills are read-only (the store refuses; duplicate first).
func (h *Handler) putMySkill(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == "import" {
		// Reserved: the path of the (removed) personal import endpoint.
		writeJSONError(w, http.StatusBadRequest, `"import" is a reserved name`)
		return
	}
	limitBody(w, r, maxSkillRequestBytes)
	var body struct {
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// ?create=1 (the New skill editor) refuses an existing name instead
	// of overwriting that skill.
	save := h.skillPkgs.SaveAuthored
	if r.URL.Query().Get("create") == "1" {
		save = h.skillPkgs.CreateAuthored
	}
	pkg, err := save(r.Context(), uid, name, body.Description, body.Body, h.skillPkgKnownTools)
	if errors.Is(err, skillpkg.ErrExists) {
		writeJSONError(w, http.StatusConflict, "you already have a skill named "+name+" — open it to edit, or pick another name")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pkg)
}

// getMySkillContent serves the editor's load: parsed frontmatter +
// body for any skill the caller owns (imported ones open read-only in
// the UI).
func (h *Handler) getMySkillContent(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	loaded, pkg, err := h.skillPkgs.ContentForOwner(r.Context(), uid, r.PathValue("id"))
	if errors.Is(err, skillpkg.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "skill not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          pkg.ID,
		"name":        pkg.Name,
		"description": loaded.Frontmatter.Description,
		"body":        loaded.Body,
		"origin":      pkg.Origin,
		"source_url":  pkg.SourceURL,
	})
}

// duplicateMySkill copies one of the caller's skills into a new
// authored (editable) skill under a new name.
func (h *Handler) duplicateMySkill(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	limitBody(w, r, maxSkillRequestBytes)
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	pkg, err := h.skillPkgs.Duplicate(r.Context(), uid, r.PathValue("id"), body.Name, h.skillPkgKnownTools)
	if errors.Is(err, skillpkg.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "skill not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, pkg)
}

// exportMySkill streams the skill directory as a zip download.
func (h *Handler) exportMySkill(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	filename, data, err := h.skillPkgs.ExportZip(r.Context(), uid, r.PathValue("id"))
	if errors.Is(err, skillpkg.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "skill not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(data)
}

func (h *Handler) deleteMySkill(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) {
		return
	}
	uid, ok := h.requireUserSession(w, r)
	if !ok {
		return
	}
	p, err := h.skillPkgs.GetOwned(r.Context(), uid, r.PathValue("id"))
	if errors.Is(err, skillpkg.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "skill not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.skillPkgs.Delete(r.Context(), p.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Shard bindings ───────────────────────────────────────────────

func (h *Handler) listShardSkills(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) || !h.requireShardStore(w) {
		return
	}
	if h.refuseShardSession(w, r) {
		return
	}
	sh, err := h.shards.GetShard(r.Context(), r.PathValue("id"))
	if err != nil || !canSeeShard(r, sh) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	items, err := h.skillPkgs.ListShardSkills(r.Context(), sh.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// putShardSkills replaces a shard's bound-skill set. Owner-scoped;
// every referenced package must exist and be enabled — binding is a
// user decision, but only over what the admin admitted.
func (h *Handler) putShardSkills(w http.ResponseWriter, r *http.Request) {
	if !h.requireSkillPkgs(w) || !h.requireShardStore(w) {
		return
	}
	if h.refuseShardSession(w, r) {
		return
	}
	sh, err := h.shards.GetShard(r.Context(), r.PathValue("id"))
	if err != nil || !canSeeShard(r, sh) {
		writeJSONError(w, http.StatusNotFound, "shard not found")
		return
	}
	limitBody(w, r, maxSkillRequestBytes)
	var body struct {
		SkillIDs []string `json:"skill_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	for _, id := range body.SkillIDs {
		p, err := h.skillPkgs.Get(r.Context(), id)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "unknown skill package "+id)
			return
		}
		if !p.Enabled() {
			writeJSONError(w, http.StatusBadRequest, "skill package "+p.Name+" is disabled")
			return
		}
		// A shard may bind instance skills and its OWNER's private
		// skills — never another user's (also enforced in SQL by
		// SetShardSkills as defense in depth).
		if p.OwnerID != "" && p.OwnerID != sh.OwnerID {
			writeJSONError(w, http.StatusBadRequest, "skill "+p.Name+" is not available to this shard's owner")
			return
		}
	}
	if err := h.skillPkgs.SetShardSkills(r.Context(), sh.ID, body.SkillIDs); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := h.skillPkgs.ListShardSkills(r.Context(), sh.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// fetchSkillArchive downloads a direct zip URL with a hard size cap
// and a short deadline. http(s) only; provenance is recorded on the
// package row by the caller.
func fetchSkillArchive(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, fmt.Errorf("url must be http(s)")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// SSRF guard: even though import is admin-only, refuse to dial
	// non-public addresses (loopback, RFC1918, cloud metadata) so an
	// admin-supplied URL can't be aimed at internal services. Shares the
	// fetch_page tool's dial-time blocklist + defeats DNS rebinding.
	client := &http.Client{Transport: fetch.SafeTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch archive: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 21<<20))
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	if len(data) > 20<<20 {
		return nil, fmt.Errorf("archive exceeds the 20MB cap")
	}
	return data, nil
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
