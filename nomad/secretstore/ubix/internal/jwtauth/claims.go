package jwtauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// leeway tolerates small clock skew when checking exp/nbf.
const leeway = 60 * time.Second

// splitJWT decodes a compact JWS into its header and claims, and returns the
// signing input (header.payload) and the raw signature.
func splitJWT(raw string) (header, claims map[string]any, signingInput, sig []byte, err error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, nil, nil, nil, errors.New("jwtauth: malformed JWT")
	}
	hb, err := b64u(parts[0])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	pb, err := b64u(parts[1])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sig, err = b64u(parts[2])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := json.Unmarshal(pb, &claims); err != nil {
		return nil, nil, nil, nil, err
	}
	return header, claims, []byte(parts[0] + "." + parts[1]), sig, nil
}

// validateClaims enforces expiry/not-before, the configured issuer, and the
// role's audience and claim bindings. now is time.Now.
func validateClaims(cfg *Config, role *Role, claims map[string]any) error {
	now := time.Now()

	exp, ok := numericClaim(claims, "exp")
	if !ok {
		return errors.New("jwtauth: token has no exp")
	}
	if now.After(exp.Add(leeway)) {
		return errors.New("jwtauth: token expired")
	}
	if nbf, ok := numericClaim(claims, "nbf"); ok && now.Add(leeway).Before(nbf) {
		return errors.New("jwtauth: token not yet valid")
	}

	if cfg.BoundIssuer != "" {
		if iss, _ := claims["iss"].(string); iss != cfg.BoundIssuer {
			return errors.New("jwtauth: issuer mismatch")
		}
	}

	if len(role.BoundAudiences) > 0 {
		if !audienceMatches(claims["aud"], role.BoundAudiences) {
			return errors.New("jwtauth: audience mismatch")
		}
	}

	for claim, want := range role.BoundClaims {
		if got := stringifyClaim(claims[claim]); got != want {
			return fmt.Errorf("jwtauth: claim %q mismatch", claim)
		}
	}
	return nil
}

// numericClaim reads a NumericDate claim (JSON number of seconds since epoch).
func numericClaim(claims map[string]any, name string) (time.Time, bool) {
	v, ok := claims[name].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(v), 0), true
}

// audienceMatches reports whether the token's aud (a string or array) contains
// any of the allowed audiences.
func audienceMatches(aud any, allowed []string) bool {
	var auds []string
	switch v := aud.(type) {
	case string:
		auds = []string{v}
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok {
				auds = append(auds, s)
			}
		}
	}
	for _, a := range auds {
		for _, want := range allowed {
			if a == want {
				return true
			}
		}
	}
	return false
}

func stringifyClaim(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}
