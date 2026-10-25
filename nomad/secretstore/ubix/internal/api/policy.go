package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/policy"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// policyWrite creates or replaces an ACL policy. The body is either Vault's API
// shape, {"policy": "<HCL or JSON policy text>"} — what Vault's clients and
// Terraform send — or the policy document itself (JSON or HCL), as earlier
// uBixVault releases documented.
func (h *Handler) policyWrite(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	doc, err := policyText(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := policy.ParseDocument(r.PathValue("name"), doc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.policies.Set(r.Context(), p); err != nil {
		writePolicyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) policyRead(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, err := h.policies.Get(r.Context(), name)
	if err != nil {
		writePolicyError(w, err)
		return
	}
	doc, err := p.Document()
	if err != nil {
		writeInternal(w, err)
		return
	}
	// A string, as in Vault: clients (Vault's own, Terraform) read it as text.
	// It is the canonical JSON form, which can be written back unchanged.
	writeData(w, map[string]any{"name": name, "policy": string(doc)})
}

// policyText extracts the policy document from a write body: the "policy"
// string when the body is Vault's {"policy": "..."} shape, else the body itself.
func policyText(body []byte) ([]byte, error) {
	var wrapped map[string]json.RawMessage
	if json.Unmarshal(body, &wrapped) != nil {
		return body, nil // HCL, or not an object: the body is the document
	}
	raw, ok := wrapped["policy"]
	if !ok {
		return body, nil // a JSON policy document ({"path": ...})
	}
	if len(wrapped) != 1 {
		return nil, errors.New(`a {"policy": ...} body must contain only "policy"`)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, errors.New(`"policy" must be a string holding the policy (HCL or JSON)`)
	}
	return []byte(text), nil
}

func (h *Handler) policyDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.policies.Delete(r.Context(), r.PathValue("name")); err != nil {
		writePolicyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) policyList(w http.ResponseWriter, r *http.Request) {
	names, err := h.policies.List(r.Context())
	if err != nil {
		writePolicyError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

// tokenCreate issues a new token with the requested policies.
func (h *Handler) tokenCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policies []string      `json:"policies"`
		TTL      vaultDuration `json:"ttl"` // optional duration; empty uses the default TTL
		// Sent on every request by Vault's own client (zero-valued when unused).
		// display_name is cosmetic and ignored; the others would change what the
		// token may do, so they are refused unless left at their zero value.
		DisplayName string `json:"display_name"`
		NumUses     int    `json:"num_uses"`
		Type        string `json:"type"`
		EntityAlias string `json:"entity_alias"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.NumUses != 0:
		writeError(w, http.StatusBadRequest, "num_uses is not supported; tokens are unlimited-use")
		return
	case req.Type != "" && req.Type != "service":
		writeError(w, http.StatusBadRequest, "only service tokens are supported")
		return
	case req.EntityAlias != "":
		writeError(w, http.StatusBadRequest, "entity_alias is not supported")
		return
	}

	var ttl time.Duration
	if req.TTL != "" {
		d, perr := time.ParseDuration(string(req.TTL))
		if perr != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "ttl must be a positive duration (e.g. \"1h\")")
			return
		}
		ttl = d
	}

	parent, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	// As in Vault (ADR D-022), a token without root or sudo on auth/token/create may only
	// give a child policies it holds itself — otherwise permission to create
	// tokens would be permission to mint any policy, including an admin one.
	sudo, err := h.hasSudo(r.Context(), parent, "auth/token/create")
	if err != nil {
		writeInternal(w, err)
		return
	}

	var tok *token.Token
	switch {
	case !sudo:
		if missing := notIn(req.Policies, parent.Policies); len(missing) > 0 {
			writeError(w, http.StatusBadRequest,
				"child policies must be subset of parent: not held by the calling token: "+strings.Join(missing, ", "))
			return
		}
		// The child cannot outlive the parent's maximum lifetime, and is revoked
		// with the parent (ADR D-022). Root- and sudo-created tokens are orphans,
		// like Vault's create-orphan, so revoking an operator's token does not
		// take down every token they ever issued.
		tok, err = h.tokens.CreateChild(r.Context(), parent, req.Policies, ttl)
	case ttl > 0:
		tok, err = h.tokens.CreateWithTTL(r.Context(), req.Policies, ttl)
	default:
		tok, err = h.tokens.Create(r.Context(), req.Policies)
	}
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenAuthResponse(tok))
}

// notIn returns the entries of want that are absent from have, in order and
// without duplicates.
func notIn(want, have []string) []string {
	held := make(map[string]bool, len(have))
	for _, p := range have {
		held[p] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, p := range want {
		if !held[p] && !seen[p] {
			missing = append(missing, p)
			seen[p] = true
		}
	}
	return missing
}

// renewSelf extends the lifetime of the calling token.
func (h *Handler) renewSelf(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	var req struct {
		Increment vaultDuration `json:"increment"` // optional duration; empty uses the default TTL
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var ttl time.Duration
	if req.Increment != "" {
		d, err := time.ParseDuration(string(req.Increment))
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "increment must be a positive duration")
			return
		}
		ttl = d
	}
	renewed, err := h.tokens.Renew(r.Context(), tok.ID, ttl)
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenAuthResponse(renewed))
}

// lookupSelf describes the calling token, in the shape of Vault's
// auth/token/lookup-self. identity_policies lists what the token's identity
// entity contributes on top of its own policies.
func (h *Handler) lookupSelf(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	identityPolicies := []string{}
	if h.identity != nil && tok.EntityID != "" {
		p, err := h.identity.PoliciesFor(r.Context(), tok.EntityID)
		if err != nil {
			writeInternal(w, err)
			return
		}
		identityPolicies = append(identityPolicies, p...)
	}
	var (
		ttl        int
		expireTime any // null for a non-expiring token, as in Vault
	)
	if !tok.ExpiresAt.IsZero() {
		if d := time.Until(tok.ExpiresAt); d > 0 {
			ttl = int(d.Seconds())
		}
		expireTime = tok.ExpiresAt.UTC().Format(time.RFC3339)
	}
	policies := tok.Policies
	if policies == nil {
		policies = []string{}
	}
	writeData(w, map[string]any{
		"id":                tok.ID,
		"policies":          policies,
		"identity_policies": identityPolicies,
		"entity_id":         tok.EntityID,
		"creation_time":     tok.CreatedTime.Unix(),
		"issue_time":        tok.CreatedTime.UTC().Format(time.RFC3339),
		"expire_time":       expireTime,
		"ttl":               ttl,
		"renewable":         !tok.ExpiresAt.IsZero(),
		"orphan":            tok.IsOrphan(),
		"type":              "service",
	})
}

// tokenAuthResponse builds the {"auth": ...} body for a token, including its
// remaining lease in seconds (0 for a non-expiring token).
func tokenAuthResponse(tok *token.Token) map[string]any {
	var leaseSeconds int
	if !tok.ExpiresAt.IsZero() {
		if d := time.Until(tok.ExpiresAt); d > 0 {
			leaseSeconds = int(d.Seconds())
		}
	}
	return map[string]any{
		"auth": map[string]any{
			"client_token":   tok.ID,
			"policies":       tok.Policies,
			"entity_id":      tok.EntityID,
			"lease_duration": leaseSeconds,
		},
	}
}

// readBody reads a size-capped request body.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return nil, false
	}
	return body, true
}

func writePolicyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrPolicyNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, policy.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
