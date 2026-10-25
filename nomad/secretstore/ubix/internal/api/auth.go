package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/policy"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// tokenHeader is the request header carrying the client token.
const tokenHeader = "X-Vault-Token" //nolint:gosec // G101: this is an HTTP header name, not a credential

// selfTokenPaths are the token endpoints every authenticated token may call on
// itself without an ACL grant (see authorize).
var selfTokenPaths = map[string]bool{
	"auth/token/lookup-self": true,
	"auth/token/renew-self":  true,
	"auth/token/revoke-self": true,
}

// ctxKey is an unexported context key type.
type ctxKey int

const tokenCtxKey ctxKey = iota

// authenticate wraps a handler so it runs only for a request that is both
// authenticated (a valid X-Vault-Token) and authorized (the token's policies
// permit the operation on the path). The authenticated token is placed on the
// request context.
func (h *Handler) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(tokenHeader)
		if id == "" {
			writeError(w, http.StatusUnauthorized, "missing authentication token")
			return
		}

		tok, err := h.tokens.Lookup(r.Context(), id)
		switch {
		case errors.Is(err, barrier.ErrSealed):
			writeError(w, http.StatusServiceUnavailable, "vault is sealed")
			return
		case errors.Is(err, token.ErrTokenNotFound), errors.Is(err, token.ErrTokenExpired):
			writeError(w, http.StatusForbidden, "permission denied")
			return
		case err != nil:
			writeInternal(w, err)
			return
		}

		allowed, err := h.authorize(r.Context(), tok, r.Method, apiPath(r))
		if err != nil {
			writeInternal(w, err)
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}

		ctx := context.WithValue(r.Context(), tokenCtxKey, tok)
		next(w, r.WithContext(ctx))
	}
}

// tokenFromContext returns the authenticated token placed on the context by
// authenticate.
func tokenFromContext(ctx context.Context) (*token.Token, bool) {
	tok, ok := ctx.Value(tokenCtxKey).(*token.Token)
	return tok, ok
}

// apiPath is the policy-space path for a request: the URL path without the
// "/v1/" version prefix (e.g. "secret/data/app").
func apiPath(r *http.Request) string {
	return strings.TrimPrefix(r.URL.Path, "/v1/")
}

// authorize reports whether tok may perform method on path. Root tokens are
// always permitted; others are evaluated against the ACL formed by their
// policies. HTTP methods map to capabilities: GET→read, LIST→list,
// DELETE→delete, POST/PUT→create or update.
func (h *Handler) authorize(ctx context.Context, tok *token.Token, method, path string) (bool, error) {
	if tok.IsRoot() {
		return true, nil
	}

	// Every token may access its own cubbyhole. Access is scoped to the token by
	// construction (the storage path derives from the token), so no ACL grant is
	// needed or would widen it — this mirrors Vault's built-in default policy.
	if path == cubbyholeMountPrefix || strings.HasPrefix(path, cubbyholeMountPrefix+"/") {
		return true, nil
	}

	// Likewise every token may look itself up, renew itself and revoke itself —
	// as under Vault's built-in default policy. Each acts only on the calling
	// token, and renewal can never pass the token's ceiling (token.Store.MaxExpiry),
	// so none of them extends what the token may do or for how long.
	if selfTokenPaths[path] {
		return true, nil
	}

	acl, err := h.aclFor(ctx, tok)
	if err != nil {
		return false, err
	}

	switch method {
	case http.MethodGet:
		return acl.Allows(path, policy.Read), nil
	case "LIST":
		return acl.Allows(path, policy.List), nil
	case http.MethodDelete:
		return acl.Allows(path, policy.Delete), nil
	case http.MethodPost, http.MethodPut:
		return acl.Allows(path, policy.Create) || acl.Allows(path, policy.Update), nil
	default:
		return false, nil
	}
}

// hasSudo reports whether tok holds the sudo capability on path. Root tokens
// always do.
func (h *Handler) hasSudo(ctx context.Context, tok *token.Token, path string) (bool, error) {
	if tok.IsRoot() {
		return true, nil
	}
	acl, err := h.aclFor(ctx, tok)
	if err != nil {
		return false, err
	}
	return acl.Allows(path, policy.Sudo), nil
}

// aclFor builds the ACL for a non-root token: its own policies plus any its
// identity entity contributes, with {{identity.*}} placeholders resolved.
func (h *Handler) aclFor(ctx context.Context, tok *token.Token) (*policy.ACL, error) {
	// A token's effective policies are its own plus any contributed by the
	// identity entity it belongs to (identity only ever adds).
	names := tok.Policies
	var resolve func(string) (string, bool)
	if h.identity != nil && tok.EntityID != "" {
		entityPolicies, err := h.identity.PoliciesFor(ctx, tok.EntityID)
		if err != nil {
			return nil, err
		}
		if len(entityPolicies) > 0 {
			names = append(append([]string{}, tok.Policies...), entityPolicies...)
		}
		// Template values for {{identity.*}} placeholders in policy paths.
		vals, err := h.identity.TemplateValues(ctx, tok.EntityID)
		if err != nil {
			return nil, err
		}
		if len(vals) > 0 {
			resolve = func(key string) (string, bool) { v, ok := vals[key]; return v, ok }
		}
	}

	var policies []*policy.Policy
	for _, name := range names {
		p, err := h.policies.Get(ctx, name)
		switch {
		case errors.Is(err, policy.ErrPolicyNotFound):
			continue // a named-but-missing policy grants nothing
		case err != nil:
			return nil, err
		}
		if resolve != nil {
			p = p.Templated(resolve) // resolve {{identity.*}} placeholders
		}
		policies = append(policies, p)
	}
	return policy.NewACL(policies...), nil
}
