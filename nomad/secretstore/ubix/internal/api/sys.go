// Package api exposes uBixVault's HTTP interface. This first cut implements the
// system endpoints for initialization and the seal/unseal lifecycle
// (docs/DESIGN.md §4). Paths mirror HashiCorp Vault's for client compatibility.
package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/approle"
	"github.com/cwolsen7905/ubixvault/internal/audit"
	"github.com/cwolsen7905/ubixvault/internal/certauth"
	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/cubbyhole"
	"github.com/cwolsen7905/ubixvault/internal/database"
	"github.com/cwolsen7905/ubixvault/internal/database/mariadb"
	"github.com/cwolsen7905/ubixvault/internal/identity"
	"github.com/cwolsen7905/ubixvault/internal/jwtauth"
	"github.com/cwolsen7905/ubixvault/internal/kubeauth"
	"github.com/cwolsen7905/ubixvault/internal/kv"
	"github.com/cwolsen7905/ubixvault/internal/ldapauth"
	"github.com/cwolsen7905/ubixvault/internal/metrics"
	"github.com/cwolsen7905/ubixvault/internal/pki"
	"github.com/cwolsen7905/ubixvault/internal/policy"
	"github.com/cwolsen7905/ubixvault/internal/quota"
	"github.com/cwolsen7905/ubixvault/internal/ratelimit"
	"github.com/cwolsen7905/ubixvault/internal/token"
	"github.com/cwolsen7905/ubixvault/internal/transit"
	"github.com/cwolsen7905/ubixvault/internal/userpass"
	"github.com/cwolsen7905/ubixvault/internal/wrapping"
)

// maxBodyBytes caps request bodies to guard against oversized payloads.
const maxBodyBytes = 1 << 20 // 1 MiB

// Storage prefixes under which the engines are mounted.
const (
	kvMountPrefix        = "secret"
	transitMountPrefix   = "transit"
	databaseMountPrefix  = "database"
	cubbyholeMountPrefix = "cubbyhole"
)

// Handler serves the HTTP API over a Core and its mounted engines. It implements
// [http.Handler].
type Handler struct {
	core           *core.Core
	kv             *kv.Engine
	transit        *transit.Engine
	database       *database.Engine
	cubbyhole      *cubbyhole.Engine
	identity       *identity.Engine
	pki            *pki.Engine
	kubernetes     *kubeauth.Method
	approle        *approle.Method
	userpass       *userpass.Method
	jwtauth        *jwtauth.Method
	certauth       *certauth.Method
	ldap           *ldapauth.Method
	wrapping       *wrapping.Store
	tokens         *token.Store
	policies       *policy.Store
	audit          *audit.Broker
	forward        http.Handler // HA: carries a standby's requests to the active replica
	auditKeyMu     sync.Mutex   // guards auditKeySet
	auditKeySet    bool         // the barrier's audit HMAC key is installed in audit
	metrics        *metrics.Metrics
	quotas         *quota.Manager // rate-limit quotas incl. the default (always non-nil)
	trustForwarded bool           // key rate limits by X-Forwarded-For
	version        string
	startTime      time.Time
	mux            *http.ServeMux
}

// Option configures a Handler.
type Option func(*Handler)

// WithAudit enables audit logging through the given broker.
func WithAudit(b *audit.Broker) Option {
	return func(h *Handler) { h.audit = b }
}

// WithForwarder has an HA standby hand requests it does not serve itself to f,
// which carries them to the active replica (cluster.Forwarder). Without one a
// standby answers them 503.
func WithForwarder(f http.Handler) Option {
	return func(h *Handler) { h.forward = f }
}

// WithVersion sets the build version reported by the health endpoint.
func WithVersion(v string) Option {
	return func(h *Handler) { h.version = v }
}

// WithRateLimit installs l as the default (global) rate-limit quota, applied to
// any path with no more-specific named quota and keyed by client. Health,
// metrics, and the console are exempt. A persisted default (sys/quotas/config)
// overrides this once the vault is unsealed.
func WithRateLimit(l *ratelimit.Limiter) Option {
	return func(h *Handler) { h.quotas.SetDefaultLimiter(l) }
}

// WithTrustForwardedFor keys rate limits by the leftmost X-Forwarded-For entry
// instead of the direct peer. Enable only behind a trusted proxy, since clients
// can otherwise spoof the header to evade limits.
func WithTrustForwardedFor() Option {
	return func(h *Handler) { h.trustForwarded = true }
}

// NewHandler returns a Handler backed by c, with the KV v2, transit, and dynamic
// database engines mounted on the core's barrier. The database engine uses the
// MariaDB reference plugin.
func NewHandler(c *core.Core, opts ...Option) *Handler {
	h := &Handler{
		core:       c,
		kv:         kv.New(c.Barrier(), kvMountPrefix),
		transit:    transit.New(c.Barrier(), transitMountPrefix),
		database:   database.New(c.Barrier(), databaseMountPrefix, mariadb.New()),
		cubbyhole:  cubbyhole.New(c.Barrier(), cubbyholeMountPrefix),
		identity:   identity.New(c.Barrier(), "identity"),
		pki:        pki.New(c.Barrier(), "pki"),
		kubernetes: kubeauth.New(c.Barrier(), c.Tokens(), "auth/kubernetes"),
		approle:    approle.New(c.Barrier(), c.Tokens(), "auth/approle"),
		userpass:   userpass.New(c.Barrier(), c.Tokens(), "auth/userpass"),
		jwtauth:    jwtauth.New(c.Barrier(), c.Tokens(), "auth/jwt"),
		certauth:   certauth.New(c.Barrier(), c.Tokens(), "auth/cert"),
		ldap:       ldapauth.New(c.Barrier(), c.Tokens(), "auth/ldap"),
		wrapping:   wrapping.NewStore(c.Barrier()),
		tokens:     c.Tokens(),
		policies:   policy.NewStore(c.Barrier()),
		metrics:    metrics.New(),
		startTime:  time.Now().UTC(),
	}
	// Quota manager reports denials to the metrics registry created above.
	h.quotas = quota.New(c.Barrier(), h.metrics.ObserveQuotaExceeded)
	// Route auth-method logins through the identity resolver so their tokens carry
	// an entity (auto-created on first login) and pick up the entity's policies.
	c.Tokens().SetAliaser(h.identity)
	// Drop everything cached from the barrier when the vault is sealed, so the
	// next unseal starts from what storage holds then.
	c.OnSeal(h.resetBarrierCaches)
	// With HA, a replica's caches may have gone stale while another replica was
	// active, and must not carry over when it stops being active.
	c.OnActive(h.resetBarrierCaches)
	c.OnStandby(h.resetBarrierCaches)

	mux := http.NewServeMux()

	// Embedded read-only web console at /ui/, with / redirecting to it.
	h.registerUI(mux)

	// System / lifecycle. These are unauthenticated by necessity: there is no
	// token before the vault exists or while it is sealed.
	mux.HandleFunc("GET /v1/sys/health", h.health)
	mux.HandleFunc("GET /v1/sys/livez", h.livez)
	mux.HandleFunc("GET /v1/sys/metrics", h.metricsEndpoint)
	mux.HandleFunc("GET /v1/sys/seal-status", h.sealStatus)
	handleWrite(mux, "/v1/sys/init", h.initialize)
	handleWrite(mux, "/v1/sys/unseal", h.unseal)
	handleWrite(mux, "/v1/sys/seal", h.authenticate(h.seal))
	mux.HandleFunc("GET /v1/sys/leader", h.leader)
	handleWrite(mux, "/v1/sys/step-down", h.authenticate(h.stepDown))

	// Root-token regeneration (recovery). Unauthenticated — authority is proven
	// by supplying a quorum of unseal shares, since the root token is lost.
	mux.HandleFunc("GET /v1/sys/generate-root/attempt", h.generateRootStatus)
	handleWrite(mux, "/v1/sys/generate-root/init", h.generateRootInit)
	mux.HandleFunc("DELETE /v1/sys/generate-root/init", h.generateRootCancel)
	handleWrite(mux, "/v1/sys/generate-root/update", h.generateRootUpdate)

	// Rekey — rotate the unseal shares by re-splitting the master key. Like
	// generate-root, unauthenticated: authority is a quorum of current shares.
	mux.HandleFunc("GET /v1/sys/rekey/init", h.rekeyStatus)
	handleWrite(mux, "/v1/sys/rekey/init", h.rekeyInit)
	mux.HandleFunc("DELETE /v1/sys/rekey/init", h.rekeyCancel)
	handleWrite(mux, "/v1/sys/rekey/update", h.rekeyUpdate)

	// KV v2 secrets engine — all endpoints require authentication.
	mux.HandleFunc("GET /v1/secret/data/{path...}", h.authenticate(h.kvRead))
	handleWrite(mux, "/v1/secret/data/{path...}", h.authenticate(h.kvWrite))
	mux.HandleFunc("DELETE /v1/secret/data/{path...}", h.authenticate(h.kvDeleteLatest))
	handleWrite(mux, "/v1/secret/delete/{path...}", h.authenticate(h.kvDeleteVersions))
	handleWrite(mux, "/v1/secret/undelete/{path...}", h.authenticate(h.kvUndelete))
	handleWrite(mux, "/v1/secret/destroy/{path...}", h.authenticate(h.kvDestroy))
	mux.HandleFunc("GET /v1/secret/metadata/{path...}", h.authenticate(h.kvReadMetadata))
	mux.HandleFunc("LIST /v1/secret/metadata/{path...}", h.authenticate(h.kvList))
	mux.HandleFunc("DELETE /v1/secret/metadata/{path...}", h.authenticate(h.kvDeleteMetadata))

	// Cubbyhole — per-token private storage. Every operation is scoped to the
	// calling token; the data is destroyed when the token is revoked.
	mux.HandleFunc("GET /v1/cubbyhole/{path...}", h.authenticate(h.cubbyRead))
	handleWrite(mux, "/v1/cubbyhole/{path...}", h.authenticate(h.cubbyWrite))
	mux.HandleFunc("LIST /v1/cubbyhole/{path...}", h.authenticate(h.cubbyList))
	mux.HandleFunc("DELETE /v1/cubbyhole/{path...}", h.authenticate(h.cubbyDelete))

	// Identity — entities and aliases (phase 1). ACL-gated like policies; a login
	// resolves its alias to an entity automatically (see SetAliaser above).
	handleWrite(mux, "/v1/identity/entity", h.authenticate(h.identityWriteEntity))
	mux.HandleFunc("LIST /v1/identity/entity/id", h.authenticate(h.identityListEntities))
	mux.HandleFunc("GET /v1/identity/entity/id/{id}", h.authenticate(h.identityReadEntityByID))
	mux.HandleFunc("DELETE /v1/identity/entity/id/{id}", h.authenticate(h.identityDeleteEntity))
	mux.HandleFunc("GET /v1/identity/entity/name/{name}", h.authenticate(h.identityReadEntityByName))
	handleWrite(mux, "/v1/identity/entity-alias", h.authenticate(h.identityWriteEntityAlias))
	mux.HandleFunc("DELETE /v1/identity/entity-alias/id/{id}", h.authenticate(h.identityDeleteEntityAlias))
	handleWrite(mux, "/v1/identity/group", h.authenticate(h.identityWriteGroup))
	mux.HandleFunc("LIST /v1/identity/group/id", h.authenticate(h.identityListGroups))
	mux.HandleFunc("GET /v1/identity/group/id/{id}", h.authenticate(h.identityReadGroupByID))
	mux.HandleFunc("DELETE /v1/identity/group/id/{id}", h.authenticate(h.identityDeleteGroup))
	mux.HandleFunc("GET /v1/identity/group/name/{name}", h.authenticate(h.identityReadGroupByName))

	// ACL policies (governed by the same ACL check; root or an explicit grant).
	handleWrite(mux, "/v1/sys/policies/acl/{name}", h.authenticate(h.policyWrite))
	mux.HandleFunc("GET /v1/sys/policies/acl/{name}", h.authenticate(h.policyRead))
	mux.HandleFunc("DELETE /v1/sys/policies/acl/{name}", h.authenticate(h.policyDelete))
	mux.HandleFunc("LIST /v1/sys/policies/acl", h.authenticate(h.policyList))

	// Resource quotas — rate-limit quotas (path-scoped request-rate limits).
	// Root/ACL-gated like policies; enforced in the request middleware.
	handleWrite(mux, "/v1/sys/quotas/rate-limit/{name}", h.authenticate(h.quotaWrite))
	mux.HandleFunc("GET /v1/sys/quotas/rate-limit/{name}", h.authenticate(h.quotaRead))
	mux.HandleFunc("DELETE /v1/sys/quotas/rate-limit/{name}", h.authenticate(h.quotaDelete))
	mux.HandleFunc("LIST /v1/sys/quotas/rate-limit", h.authenticate(h.quotaList))
	mux.HandleFunc("GET /v1/sys/quotas/config", h.authenticate(h.quotaConfigRead))
	handleWrite(mux, "/v1/sys/quotas/config", h.authenticate(h.quotaConfigWrite))
	handleWrite(mux, "/v1/sys/quotas/lease-count/{name}", h.authenticate(h.leaseQuotaWrite))
	mux.HandleFunc("GET /v1/sys/quotas/lease-count/{name}", h.authenticate(h.leaseQuotaRead))
	mux.HandleFunc("DELETE /v1/sys/quotas/lease-count/{name}", h.authenticate(h.leaseQuotaDelete))
	mux.HandleFunc("LIST /v1/sys/quotas/lease-count", h.authenticate(h.leaseQuotaList))

	// Token creation, self-lookup, renewal, and revocation (revoke cascades to the token's
	// dynamic-database leases and destroys its cubbyhole).
	handleWrite(mux, "/v1/auth/token/create", h.authenticate(h.tokenCreate))
	mux.HandleFunc("GET /v1/auth/token/lookup-self", h.authenticate(h.lookupSelf))
	handleWrite(mux, "/v1/auth/token/renew-self", h.authenticate(h.renewSelf))
	handleWrite(mux, "/v1/auth/token/revoke-self", h.authenticate(h.tokenRevokeSelf))

	// Response wrapping: wrap a payload in a single-use, TTL'd token, and unwrap
	// it exactly once. Both require a token; the wrapping token is passed in the
	// unwrap body.
	handleWrite(mux, "/v1/sys/wrapping/wrap", h.authenticate(h.sysWrappingWrap))
	handleWrite(mux, "/v1/sys/wrapping/unwrap", h.authenticate(h.sysWrappingUnwrap))

	// Transit engine (encryption-as-a-service).
	handleWrite(mux, "/v1/transit/keys/{name}", h.authenticate(h.transitCreateKey))
	mux.HandleFunc("GET /v1/transit/keys/{name}", h.authenticate(h.transitReadKey))
	mux.HandleFunc("DELETE /v1/transit/keys/{name}", h.authenticate(h.transitDeleteKey))
	mux.HandleFunc("LIST /v1/transit/keys", h.authenticate(h.transitListKeys))
	handleWrite(mux, "/v1/transit/keys/{name}/rotate", h.authenticate(h.transitRotateKey))
	handleWrite(mux, "/v1/transit/encrypt/{name}", h.authenticate(h.transitEncrypt))
	handleWrite(mux, "/v1/transit/decrypt/{name}", h.authenticate(h.transitDecrypt))
	handleWrite(mux, "/v1/transit/rewrap/{name}", h.authenticate(h.transitRewrap))
	handleWrite(mux, "/v1/transit/datakey/{mode}/{name}", h.authenticate(h.transitDataKey))
	handleWrite(mux, "/v1/transit/hmac/{name}", h.authenticate(h.transitHMAC))
	handleWrite(mux, "/v1/transit/sign/{name}", h.authenticate(h.transitSign))
	handleWrite(mux, "/v1/transit/verify/{name}", h.authenticate(h.transitVerify))

	// Dynamic database secrets engine.
	handleWrite(mux, "/v1/database/config", h.authenticate(h.dbConfigure))
	mux.HandleFunc("GET /v1/database/config", h.authenticate(h.dbConfigStatus))
	handleWrite(mux, "/v1/database/roles/{name}", h.authenticate(h.dbWriteRole))
	mux.HandleFunc("GET /v1/database/roles/{name}", h.authenticate(h.dbReadRole))
	mux.HandleFunc("LIST /v1/database/roles", h.authenticate(h.dbListRoles))
	mux.HandleFunc("DELETE /v1/database/roles/{name}", h.authenticate(h.dbDeleteRole))
	mux.HandleFunc("GET /v1/database/creds/{name}", h.authenticate(h.dbCredentials))

	// PKI secrets engine — internal CA and short-lived certificate issuance.
	handleWrite(mux, "/v1/pki/root/generate/internal", h.authenticate(h.pkiGenerateRoot))
	mux.HandleFunc("GET /v1/pki/ca", h.authenticate(h.pkiReadCA))
	handleWrite(mux, "/v1/pki/roles/{name}", h.authenticate(h.pkiWriteRole))
	mux.HandleFunc("GET /v1/pki/roles/{name}", h.authenticate(h.pkiReadRole))
	mux.HandleFunc("LIST /v1/pki/roles", h.authenticate(h.pkiListRoles))
	mux.HandleFunc("DELETE /v1/pki/roles/{name}", h.authenticate(h.pkiDeleteRole))
	handleWrite(mux, "/v1/pki/issue/{role}", h.authenticate(h.pkiIssue))

	// Lease management (currently database leases only).
	handleWrite(mux, "/v1/sys/leases/revoke", h.authenticate(h.leaseRevoke))
	handleWrite(mux, "/v1/sys/leases/renew", h.authenticate(h.leaseRenew))
	handleWrite(mux, "/v1/sys/leases/lookup", h.authenticate(h.leaseLookup))

	// Backup: stream a snapshot of the encrypted store (root or an explicit grant).
	handleWrite(mux, "/v1/sys/snapshot", h.authenticate(h.snapshot))

	// Kubernetes auth method. login is unauthenticated (the ServiceAccount token
	// IS the credential); config and role management require authentication.
	handleWrite(mux, "/v1/auth/kubernetes/config", h.authenticate(h.k8sConfigure))
	handleWrite(mux, "/v1/auth/kubernetes/role/{name}", h.authenticate(h.k8sWriteRole))
	mux.HandleFunc("GET /v1/auth/kubernetes/role/{name}", h.authenticate(h.k8sReadRole))
	mux.HandleFunc("LIST /v1/auth/kubernetes/role", h.authenticate(h.k8sListRoles))
	mux.HandleFunc("DELETE /v1/auth/kubernetes/role/{name}", h.authenticate(h.k8sDeleteRole))
	handleWrite(mux, "/v1/auth/kubernetes/login", h.k8sLogin)

	// AppRole auth method. login is unauthenticated (role_id + secret_id are the
	// credential); role and secret-id management require authentication.
	handleWrite(mux, "/v1/auth/approle/role/{name}", h.authenticate(h.approleWriteRole))
	mux.HandleFunc("GET /v1/auth/approle/role/{name}", h.authenticate(h.approleReadRole))
	mux.HandleFunc("LIST /v1/auth/approle/role", h.authenticate(h.approleListRoles))
	mux.HandleFunc("DELETE /v1/auth/approle/role/{name}", h.authenticate(h.approleDeleteRole))
	mux.HandleFunc("GET /v1/auth/approle/role/{name}/role-id", h.authenticate(h.approleReadRoleID))
	handleWrite(mux, "/v1/auth/approle/role/{name}/secret-id", h.authenticate(h.approleGenerateSecretID))
	handleWrite(mux, "/v1/auth/approle/login", h.approleLogin)

	// Userpass auth method. login is unauthenticated (the password is the
	// credential); user management requires authentication.
	handleWrite(mux, "/v1/auth/userpass/users/{username}", h.authenticate(h.userpassWriteUser))
	mux.HandleFunc("GET /v1/auth/userpass/users/{username}", h.authenticate(h.userpassReadUser))
	mux.HandleFunc("LIST /v1/auth/userpass/users", h.authenticate(h.userpassListUsers))
	mux.HandleFunc("DELETE /v1/auth/userpass/users/{username}", h.authenticate(h.userpassDeleteUser))
	handleWrite(mux, "/v1/auth/userpass/login/{username}", h.userpassLogin)

	// JWT/OIDC auth: configure signature validation and roles (authenticated),
	// then exchange a signed JWT for a token (unauthenticated).
	handleWrite(mux, "/v1/auth/jwt/config", h.authenticate(h.jwtConfigure))
	handleWrite(mux, "/v1/auth/jwt/role/{name}", h.authenticate(h.jwtWriteRole))
	mux.HandleFunc("GET /v1/auth/jwt/role/{name}", h.authenticate(h.jwtReadRole))
	mux.HandleFunc("LIST /v1/auth/jwt/role", h.authenticate(h.jwtListRoles))
	mux.HandleFunc("DELETE /v1/auth/jwt/role/{name}", h.authenticate(h.jwtDeleteRole))
	handleWrite(mux, "/v1/auth/jwt/login", h.jwtLogin)

	// TLS certificate auth: define trusted cert roles (authenticated), then log in
	// by presenting a matching mTLS client certificate (unauthenticated).
	handleWrite(mux, "/v1/auth/cert/certs/{name}", h.authenticate(h.certWriteCert))
	mux.HandleFunc("GET /v1/auth/cert/certs/{name}", h.authenticate(h.certReadCert))
	mux.HandleFunc("LIST /v1/auth/cert/certs", h.authenticate(h.certListCerts))
	mux.HandleFunc("DELETE /v1/auth/cert/certs/{name}", h.authenticate(h.certDeleteCert))
	handleWrite(mux, "/v1/auth/cert/login", h.certLogin)

	// LDAP/AD auth: configure the directory and group→policy maps (authenticated),
	// then log in with a directory username + password (unauthenticated).
	handleWrite(mux, "/v1/auth/ldap/config", h.authenticate(h.ldapConfigure))
	mux.HandleFunc("GET /v1/auth/ldap/config", h.authenticate(h.ldapReadConfig))
	handleWrite(mux, "/v1/auth/ldap/groups/{name}", h.authenticate(h.ldapWriteGroup))
	mux.HandleFunc("GET /v1/auth/ldap/groups/{name}", h.authenticate(h.ldapReadGroup))
	mux.HandleFunc("LIST /v1/auth/ldap/groups", h.authenticate(h.ldapListGroups))
	mux.HandleFunc("DELETE /v1/auth/ldap/groups/{name}", h.authenticate(h.ldapDeleteGroup))
	handleWrite(mux, "/v1/auth/ldap/login/{username}", h.ldapLogin)

	h.mux = mux
	for _, opt := range opts {
		opt(h)
	}
	// Register metrics after options so gauges like build_info capture the
	// version set by WithVersion.
	h.registerMetrics()
	return h
}

// RunLeaseSweeper periodically revokes expired database leases until ctx is
// cancelled. It sweeps only on the active replica — with HA, standbys would
// race it to DROP USER and their writes are fenced anyway. Errors (including
// "sealed") are ignored; the next tick retries.
func (h *Handler) RunLeaseSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.core.Active() {
				_, _ = h.database.RevokeExpired(ctx)
			}
		}
	}
}

// tokenSweepBatch caps how many expired tokens one sweep deletes, so a large
// backlog is worked off over several sweeps instead of in one long burst.
const tokenSweepBatch = 20000

// RunTokenSweeper deletes expired tokens every interval (on the active replica
// only), destroying each one's cubbyhole and revoking its dynamic-database
// leases first — what revoke-self does for a live token.
func (h *Handler) RunTokenSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.core.Active() {
				_, _ = h.sweepExpiredTokens(ctx)
			}
		}
	}
}

func (h *Handler) sweepExpiredTokens(ctx context.Context) (int, error) {
	return h.tokens.SweepExpired(ctx, tokenSweepBatch, h.cleanupToken)
}

// initRequest accepts every field of Vault's sys/init request, because Vault's
// own client sends them all (zero-valued when unused). Fields uBixVault does not
// implement are accepted only at their zero value — silently ignoring, say,
// pgp_keys would hand back plaintext shares to an operator who asked for
// encrypted ones.
type initRequest struct {
	SecretShares      int      `json:"secret_shares"`
	SecretThreshold   int      `json:"secret_threshold"`
	StoredShares      int      `json:"stored_shares"`
	PGPKeys           []string `json:"pgp_keys"`
	RootTokenPGPKey   string   `json:"root_token_pgp_key"`
	RecoveryShares    int      `json:"recovery_shares"`
	RecoveryThreshold int      `json:"recovery_threshold"`
	RecoveryPGPKeys   []string `json:"recovery_pgp_keys"`
}

type initResponse struct {
	Keys               []string `json:"keys"`                           // hex-encoded unseal shares (Shamir mode)
	KeysBase64         []string `json:"keys_base64"`                    // same shares, base64
	RecoveryKeys       []string `json:"recovery_keys,omitempty"`        // hex-encoded recovery shares (auto-unseal mode)
	RecoveryKeysBase64 []string `json:"recovery_keys_base64,omitempty"` // same recovery shares, base64
	RootToken          string   `json:"root_token"`                     // initial root token, shown once
}

type unsealRequest struct {
	Key     string `json:"key"`     // a single unseal share, hex or base64
	Reset   bool   `json:"reset"`   // Vault: discard shares entered so far — not supported
	Migrate bool   `json:"migrate"` // Vault: seal migration — not supported
}

type statusResponse struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	Type        string `json:"type,omitempty"` // "shamir" or "auto"
	T           int    `json:"t"`              // threshold
	N           int    `json:"n"`              // total shares
	Progress    int    `json:"progress"`
}

type errorResponse struct {
	Errors []string `json:"errors"`
}

func (h *Handler) sealStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.core.Status(r.Context())
	if err != nil {
		writeInternal(w, err)
		return
	}
	writeStatus(w, st)
}

func (h *Handler) initialize(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case len(req.PGPKeys) > 0 || req.RootTokenPGPKey != "" || len(req.RecoveryPGPKeys) > 0:
		writeError(w, http.StatusBadRequest, "PGP-encrypted keys (pgp_keys, root_token_pgp_key, recovery_pgp_keys) are not supported")
		return
	case req.StoredShares != 0:
		writeError(w, http.StatusBadRequest, "stored_shares is not supported")
		return
	}
	cfg := core.InitConfig{SecretShares: req.SecretShares, SecretThreshold: req.SecretThreshold}
	// With auto-unseal the shares are recovery keys. Vault's clients describe
	// them with recovery_shares/recovery_threshold (and send secret_* as well),
	// so prefer those when given; with Shamir they are ignored, as in Vault.
	if h.core.AutoUnsealEnabled() && (req.RecoveryShares != 0 || req.RecoveryThreshold != 0) {
		cfg = core.InitConfig{SecretShares: req.RecoveryShares, SecretThreshold: req.RecoveryThreshold}
	}
	res, err := h.core.Initialize(r.Context(), cfg)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, core.ErrAlreadyInitialized) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}

	resp := initResponse{
		Keys:       make([]string, len(res.Keys)),
		KeysBase64: make([]string, len(res.Keys)),
		RootToken:  res.RootToken,
	}
	for i, k := range res.Keys {
		resp.Keys[i] = hex.EncodeToString(k)
		resp.KeysBase64[i] = base64.StdEncoding.EncodeToString(k)
	}
	for _, k := range res.RecoveryKeys {
		resp.RecoveryKeys = append(resp.RecoveryKeys, hex.EncodeToString(k))
		resp.RecoveryKeysBase64 = append(resp.RecoveryKeysBase64, base64.StdEncoding.EncodeToString(k))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) unseal(w http.ResponseWriter, r *http.Request) {
	var req unsealRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reset || req.Migrate {
		writeError(w, http.StatusBadRequest, "unseal reset and migrate are not supported")
		return
	}
	share, err := decodeShare(req.Key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "key must be valid hex or base64")
		return
	}

	st, err := h.core.Unseal(r.Context(), share)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeStatus(w, st)
}

func (h *Handler) seal(w http.ResponseWriter, _ *http.Request) {
	// Routed through authenticate: needs a token whose policy grants sys/seal.
	h.core.Seal()
	w.WriteHeader(http.StatusNoContent)
}

// decodeShare accepts a share encoded as hex or standard base64.
func decodeShare(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// decodeJSON reads a JSON body into v, writing a 400 on failure. It returns
// false if the caller should stop.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func writeStatus(w http.ResponseWriter, st *core.SealStatus) {
	writeJSON(w, http.StatusOK, statusResponse{
		Initialized: st.Initialized,
		Sealed:      st.Sealed,
		Type:        st.Type,
		T:           st.Threshold,
		N:           st.Shares,
		Progress:    st.Progress,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msgs ...string) {
	writeJSON(w, status, errorResponse{Errors: msgs})
}

// writeInternal handles an unexpected server-side error: it logs the detail for
// the operator but returns only a generic message to the client, so internal
// error strings (storage paths, wrapped chains) are not disclosed.
func writeInternal(w http.ResponseWriter, err error) {
	log.Printf("api: internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// handleWrite registers handler for a write endpoint under both POST and PUT.
// HashiCorp Vault treats the two identically on every write path, and its own
// Go client — used by the vault CLI and by External Secrets Operator — sends PUT
// (e.g. PUT auth/kubernetes/login), while curl examples and other clients send
// POST. Registering one without the other breaks one family of clients with a
// 405, so every write route goes through here (TestWriteRoutesAcceptPutAndPost).
func handleWrite(mux *http.ServeMux, path string, handler http.HandlerFunc) {
	mux.HandleFunc("POST "+path, handler)
	mux.HandleFunc("PUT "+path, handler)
}
