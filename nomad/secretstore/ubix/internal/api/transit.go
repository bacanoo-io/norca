package api

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/transit"
)

// transitCreateKey creates a key. An optional {"type":"..."} body selects the
// key type (default aes256-gcm96).
func (h *Handler) transitCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type       string `json:"type"`
		Derived    bool   `json:"derived"`
		Convergent bool   `json:"convergent"`
	}
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	info, err := h.transit.CreateKeyWithOptions(r.Context(), r.PathValue("name"), req.Type, transit.KeyOptions{
		Derived:    req.Derived,
		Convergent: req.Convergent,
	})
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, info)
}

// decodeContext decodes the optional base64 "context" field for derived keys.
// An empty string yields a nil context.
func decodeContext(w http.ResponseWriter, s string) ([]byte, bool) {
	if s == "" {
		return nil, true
	}
	dctx, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		writeError(w, http.StatusBadRequest, "context must be base64")
		return nil, false
	}
	return dctx, true
}

func (h *Handler) transitRotateKey(w http.ResponseWriter, r *http.Request) {
	info, err := h.transit.Rotate(r.Context(), r.PathValue("name"))
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, info)
}

func (h *Handler) transitReadKey(w http.ResponseWriter, r *http.Request) {
	info, err := h.transit.ReadKey(r.Context(), r.PathValue("name"))
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, info)
}

func (h *Handler) transitListKeys(w http.ResponseWriter, r *http.Request) {
	names, err := h.transit.ListKeys(r.Context())
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) transitDeleteKey(w http.ResponseWriter, r *http.Request) {
	if err := h.transit.DeleteKey(r.Context(), r.PathValue("name")); err != nil {
		writeTransitError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transitEncrypt takes base64 plaintext and returns transit ciphertext.
func (h *Handler) transitEncrypt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Plaintext string `json:"plaintext"`
		Context   string `json:"context"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	plaintext, err := base64.StdEncoding.DecodeString(req.Plaintext)
	if err != nil {
		writeError(w, http.StatusBadRequest, "plaintext must be base64")
		return
	}
	dctx, ok := decodeContext(w, req.Context)
	if !ok {
		return
	}
	ciphertext, err := h.transit.EncryptWithContext(r.Context(), r.PathValue("name"), plaintext, dctx)
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"ciphertext": ciphertext})
}

// transitDecrypt takes transit ciphertext and returns base64 plaintext.
func (h *Handler) transitDecrypt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ciphertext string `json:"ciphertext"`
		Context    string `json:"context"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	dctx, ok := decodeContext(w, req.Context)
	if !ok {
		return
	}
	plaintext, err := h.transit.DecryptWithContext(r.Context(), r.PathValue("name"), req.Ciphertext, dctx)
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plaintext)})
}

// transitRewrap re-encrypts a ciphertext under the key's latest version.
func (h *Handler) transitRewrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ciphertext string `json:"ciphertext"`
		Context    string `json:"context"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	dctx, ok := decodeContext(w, req.Context)
	if !ok {
		return
	}
	ciphertext, err := h.transit.RewrapWithContext(r.Context(), r.PathValue("name"), req.Ciphertext, dctx)
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"ciphertext": ciphertext})
}

// transitDataKey generates a random data key wrapped under the named key. The
// {mode} path segment is "plaintext" (return the key and its wrapped form) or
// "wrapped" (return only the wrapped form). An optional {"bits":N} body selects
// the key size (128/256/512; default 256).
func (h *Handler) transitDataKey(w http.ResponseWriter, r *http.Request) {
	mode := r.PathValue("mode")
	if mode != "plaintext" && mode != "wrapped" {
		writeError(w, http.StatusBadRequest, "mode must be plaintext or wrapped")
		return
	}
	req := struct {
		Bits int `json:"bits"`
	}{Bits: 256}
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	plaintext, wrapped, err := h.transit.GenerateDataKey(r.Context(), r.PathValue("name"), req.Bits)
	if err != nil {
		writeTransitError(w, err)
		return
	}
	out := map[string]any{"ciphertext": wrapped}
	if mode == "plaintext" {
		out["plaintext"] = base64.StdEncoding.EncodeToString(plaintext)
	}
	writeData(w, out)
}

// transitHMAC computes an HMAC over base64 input using the named key.
func (h *Handler) transitHMAC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input     string `json:"input"`
		Algorithm string `json:"algorithm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	input, err := base64.StdEncoding.DecodeString(req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "input must be base64")
		return
	}
	mac, err := h.transit.HMAC(r.Context(), r.PathValue("name"), input, req.Algorithm)
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"hmac": mac})
}

// transitSign signs base64 input with the named signing key.
func (h *Handler) transitSign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input     string `json:"input"`
		Algorithm string `json:"algorithm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	input, err := base64.StdEncoding.DecodeString(req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "input must be base64")
		return
	}
	sig, err := h.transit.Sign(r.Context(), r.PathValue("name"), input, req.Algorithm)
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"signature": sig})
}

// transitVerify checks a signature or an HMAC over base64 input against the named
// key. Exactly one of "signature" or "hmac" must be supplied.
func (h *Handler) transitVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input     string `json:"input"`
		HMAC      string `json:"hmac"`
		Signature string `json:"signature"`
		Algorithm string `json:"algorithm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.HMAC == "") == (req.Signature == "") {
		writeError(w, http.StatusBadRequest, "exactly one of hmac or signature is required")
		return
	}
	input, err := base64.StdEncoding.DecodeString(req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "input must be base64")
		return
	}
	name := r.PathValue("name")
	var valid bool
	if req.Signature != "" {
		valid, err = h.transit.Verify(r.Context(), name, input, req.Signature, req.Algorithm)
	} else {
		valid, err = h.transit.VerifyHMAC(r.Context(), name, input, req.HMAC, req.Algorithm)
	}
	if err != nil {
		writeTransitError(w, err)
		return
	}
	writeData(w, map[string]any{"valid": valid})
}

func writeTransitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, transit.ErrKeyNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, transit.ErrKeyExists),
		errors.Is(err, transit.ErrInvalidName),
		errors.Is(err, transit.ErrInvalidCiphertext),
		errors.Is(err, transit.ErrInvalidDataKeyBits),
		errors.Is(err, transit.ErrInvalidAlgorithm),
		errors.Is(err, transit.ErrInvalidKeyType),
		errors.Is(err, transit.ErrKeyTypeMismatch),
		errors.Is(err, transit.ErrContextRequired),
		errors.Is(err, transit.ErrContextNotAllowed):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
