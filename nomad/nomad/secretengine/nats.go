// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretengine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/hashicorp/nomad/nomad/structs"
)

// The NATS backend (decided 2026-10-25, the first backend of the engine):
// a credential is a fresh NKey user with a user JWT signed by an account
// signing key, limited to the subjects of the role and expiring at the end
// of the lease's max_ttl. A lease that ends earlier (not renewed, its
// allocation stopped, revoked by an operator) is revoked through the
// account's revocation list.
//
// The NATS deployment runs in operator mode (JWT authentication). The engine
// owns the account JWT it was given: it pushes it to the servers re-signed
// with an operator signing key, with the engine's account signing keys and
// the revocations of its leases added, through the system account
// ($SYS.REQ.ACCOUNT.<account>.CLAIMS.UPDATE). Everything pushed is computed
// from the engine object and the leases, so a new leader pushes the same
// JWT. The admin credential of the engine is one built-in secret with the
// items:
//
//   - account_signing_seed: the seed (SA...) of an account signing key that
//     signs the user JWTs; rotate-root replaces it;
//   - operator_signing_seed: the seed (SO...) of a signing key of the
//     operator, which signs the account JWT;
//   - system_creds: the credentials file of a user of the system account,
//     used to push the account JWT.
//
// The servers need an account resolver that accepts updates: the full (nats)
// resolver stores them; the memory resolver applies them to the accounts in
// use until the server restarts (the leader pushes again periodically).

const (
	// NATSItemAccountSigningSeed, NATSItemOperatorSigningSeed and
	// NATSItemSystemCreds are the items of a nats engine's admin
	// credential.
	NATSItemAccountSigningSeed  = "account_signing_seed"
	NATSItemOperatorSigningSeed = "operator_signing_seed"
	NATSItemSystemCreds         = "system_creds"

	// The items of a NATS credential: the credentials file and its parts.
	NATSItemCreds   = "creds"
	NATSItemJWT     = "jwt"
	NATSItemSeed    = "seed"
	NATSItemUser    = "user"
	NATSDefaultFile = "nats.creds"

	// natsClaimsUpdateSubject is the system account subject of account JWT
	// updates; both the full and the memory resolver serve it.
	natsClaimsUpdateSubject = "$SYS.REQ.ACCOUNT.%s.CLAIMS.UPDATE"

	// natsPushTimeout bounds the connection and the update request.
	natsPushTimeout = 10 * time.Second
)

// NATS is the backend of nats engines.
type NATS struct{}

func (*NATS) Type() string { return structs.SecretEngineTypeNATS }

// natsAdmin is the parsed admin credential of a nats engine.
type natsAdmin struct {
	accountSigner  nkeys.KeyPair
	accountKey     string
	operatorSigner nkeys.KeyPair
	systemJWT      string
	systemSeed     []byte
}

// parseNATSAdmin parses the admin credential. Errors never contain the
// values.
func parseNATSAdmin(cred structs.SecretItems) (*natsAdmin, error) {
	a := &natsAdmin{}
	var err error
	if a.accountSigner, err = parseNATSSeed(cred, NATSItemAccountSigningSeed, nkeys.PrefixByteAccount); err != nil {
		return nil, err
	}
	if a.accountKey, err = a.accountSigner.PublicKey(); err != nil {
		return nil, fmt.Errorf("item %s: invalid account seed", NATSItemAccountSigningSeed)
	}
	if a.operatorSigner, err = parseNATSSeed(cred, NATSItemOperatorSigningSeed, nkeys.PrefixByteOperator); err != nil {
		return nil, err
	}
	creds := cred[NATSItemSystemCreds]
	if creds == "" {
		return nil, fmt.Errorf("the admin credential lacks the item %s", NATSItemSystemCreds)
	}
	if a.systemJWT, err = jwt.ParseDecoratedJWT([]byte(creds)); err != nil {
		return nil, fmt.Errorf("item %s: not a NATS credentials file", NATSItemSystemCreds)
	}
	kp, err := jwt.ParseDecoratedUserNKey([]byte(creds))
	if err != nil {
		return nil, fmt.Errorf("item %s: no user seed in the credentials file", NATSItemSystemCreds)
	}
	if a.systemSeed, err = kp.Seed(); err != nil {
		return nil, fmt.Errorf("item %s: no user seed in the credentials file", NATSItemSystemCreds)
	}
	return a, nil
}

func parseNATSSeed(cred structs.SecretItems, item string, prefix nkeys.PrefixByte) (nkeys.KeyPair, error) {
	seed := strings.TrimSpace(cred[item])
	if seed == "" {
		return nil, fmt.Errorf("the admin credential lacks the item %s", item)
	}
	p, _, err := nkeys.DecodeSeed([]byte(seed))
	if err != nil || p != prefix {
		return nil, fmt.Errorf("item %s: not a NATS %s seed", item, prefix)
	}
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return nil, fmt.Errorf("item %s: not a NATS %s seed", item, prefix)
	}
	return kp, nil
}

// natsAccountClaims decodes the engine's account JWT.
func natsAccountClaims(engine *structs.SecretEngine) (*jwt.AccountClaims, error) {
	if engine.NATS == nil {
		return nil, errors.New("the engine has no nats configuration")
	}
	ac, err := jwt.DecodeAccountClaims(strings.TrimSpace(engine.NATS.AccountJWT))
	if err != nil {
		return nil, fmt.Errorf("invalid account JWT: %v", err)
	}
	if !nkeys.IsValidPublicAccountKey(ac.Subject) {
		return nil, errors.New("invalid account JWT: the subject is not an account")
	}
	return ac, nil
}

// Validate checks the account JWT and the admin credential.
func (*NATS) Validate(engine *structs.SecretEngine, cred structs.SecretItems) error {
	if _, err := natsAccountClaims(engine); err != nil {
		return err
	}
	if _, err := parseNATSAdmin(cred); err != nil {
		return err
	}
	if engine.NATS.CACert != "" {
		if _, err := natsCAPool(engine.NATS.CACert); err != nil {
			return err
		}
	}
	return nil
}

// Issue creates a NATS user for the role: a fresh NKey and a user JWT signed
// by the account signing key, with the role's permissions, expiring at the
// end of max_ttl.
func (*NATS) Issue(ctx context.Context, req *IssueRequest) (*Credential, error) {
	ac, err := natsAccountClaims(req.Engine)
	if err != nil {
		return nil, err
	}
	admin, err := parseNATSAdmin(req.Credential)
	if err != nil {
		return nil, err
	}
	role := req.Role.NATS
	if role == nil {
		return nil, fmt.Errorf("role %q has no nats permissions", req.Role.Name)
	}

	user, err := nkeys.CreateUser()
	if err != nil {
		return nil, err
	}
	userKey, err := user.PublicKey()
	if err != nil {
		return nil, err
	}
	seed, err := user.Seed()
	if err != nil {
		return nil, err
	}

	uc := jwt.NewUserClaims(userKey)
	uc.Name = req.Name
	uc.IssuerAccount = ac.Subject
	uc.IssuedAt = req.IssueTime.Unix()
	uc.Expires = req.MaxExpireTime.Unix()
	uc.Pub.Allow.Add(role.Publish...)
	uc.Pub.Deny.Add(role.PublishDeny...)
	uc.Sub.Allow.Add(role.Subscribe...)
	uc.Sub.Deny.Add(role.SubscribeDeny...)
	if role.AllowResponses {
		uc.Resp = &jwt.ResponsePermission{MaxMsgs: 1}
	}
	userJWT, err := uc.Encode(admin.accountSigner)
	if err != nil {
		return nil, fmt.Errorf("failed to sign the user JWT: %v", err)
	}
	creds, err := jwt.FormatUserConfig(userJWT, seed)
	if err != nil {
		return nil, err
	}
	return &Credential{
		Items: structs.SecretItems{
			NATSItemCreds: string(creds),
			NATSItemJWT:   userJWT,
			NATSItemSeed:  string(seed),
			NATSItemUser:  userKey,
		},
		BackendRef: userKey,
		SigningKey: admin.accountKey,
		File:       NATSDefaultFile,
		FileItem:   NATSItemCreds,
	}, nil
}

// NATSAccountJWT returns the account JWT the engine pushes: the operator's
// account JWT with the engine's signing keys (the current one and the
// retired ones still signing leases, without the removed ones) and the
// revocations of the leases, signed by the operator signing key.
func NATSAccountJWT(req *SyncRequest) (string, error) {
	ac, err := natsAccountClaims(req.Engine)
	if err != nil {
		return "", err
	}
	admin, err := parseNATSAdmin(req.Credential)
	if err != nil {
		return "", err
	}
	if ac.SigningKeys == nil {
		ac.SigningKeys = jwt.SigningKeys{}
	}
	if st := req.Engine.State; st != nil {
		ac.SigningKeys.Remove(st.RemovedKeys...)
		ac.SigningKeys.Add(st.RetiredKeys...)
	}
	ac.SigningKeys.Add(admin.accountKey)
	if ac.Revocations == nil {
		ac.Revocations = jwt.RevocationList{}
	}
	for key, at := range req.Revoked {
		ac.RevokeAt(key, at)
	}
	signed, err := ac.Encode(admin.operatorSigner)
	if err != nil {
		return "", fmt.Errorf("failed to sign the account JWT: %v", err)
	}
	return signed, nil
}

// Sync pushes the account JWT to the NATS servers.
func (*NATS) Sync(ctx context.Context, req *SyncRequest) error {
	signed, err := NATSAccountJWT(req)
	if err != nil {
		return err
	}
	return natsPush(ctx, req, signed)
}

// RotateRoot creates a new account signing key and pushes the account JWT
// with it next to the current key. The current key is retired by the caller
// and removed from the account once the leases it signed have ended.
func (*NATS) RotateRoot(ctx context.Context, req *SyncRequest) (*RotateResult, error) {
	admin, err := parseNATSAdmin(req.Credential)
	if err != nil {
		return nil, err
	}
	kp, err := nkeys.CreateAccount()
	if err != nil {
		return nil, err
	}
	newKey, err := kp.PublicKey()
	if err != nil {
		return nil, err
	}
	seed, err := kp.Seed()
	if err != nil {
		return nil, err
	}

	cred := req.Credential.Copy()
	cred[NATSItemAccountSigningSeed] = string(seed)
	engine := req.Engine.Copy()
	if engine.State == nil {
		engine.State = &structs.SecretEngineState{}
	}
	if !slices.Contains(engine.State.RetiredKeys, admin.accountKey) {
		engine.State.RetiredKeys = append(engine.State.RetiredKeys, admin.accountKey)
	}
	engine.State.RemovedKeys = slices.DeleteFunc(engine.State.RemovedKeys, func(k string) bool { return k == newKey })
	next := &SyncRequest{Engine: engine, Credential: cred, Revoked: req.Revoked}
	signed, err := NATSAccountJWT(next)
	if err != nil {
		return nil, err
	}
	if err := natsPush(ctx, next, signed); err != nil {
		return nil, err
	}
	return &RotateResult{Credential: cred, OldKey: admin.accountKey, NewKey: newKey}, nil
}

// natsCAPool parses the CA certificate of the engine.
func natsCAPool(pem string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, errors.New("nats: the CA certificate is not a PEM encoded certificate")
	}
	return pool, nil
}

// natsClaimUpdateResponse is the response of the servers to an account JWT
// update.
type natsClaimUpdateResponse struct {
	Data *struct {
		Message string `json:"message"`
	} `json:"data"`
	Error *struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"error"`
}

// natsPush sends the account JWT to the servers with the system account
// user of the admin credential.
func natsPush(ctx context.Context, req *SyncRequest, accountJWT string) error {
	admin, err := parseNATSAdmin(req.Credential)
	if err != nil {
		return err
	}
	ac, err := natsAccountClaims(req.Engine)
	if err != nil {
		return err
	}
	opts := []nats.Option{
		nats.Name("norca-secret-engine"),
		nats.UserJWTAndSeed(admin.systemJWT, string(admin.systemSeed)),
		nats.Timeout(natsPushTimeout),
		nats.NoReconnect(),
	}
	if pem := req.Engine.NATS.CACert; pem != "" {
		pool, err := natsCAPool(pem)
		if err != nil {
			return err
		}
		opts = append(opts, nats.Secure(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	}
	nc, err := nats.Connect(strings.Join(req.Engine.NATS.Servers, ","), opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to the NATS servers with the system account user: %v", err)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(ctx, natsPushTimeout)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, fmt.Sprintf(natsClaimsUpdateSubject, ac.Subject), []byte(accountJWT))
	if err != nil {
		return fmt.Errorf("account JWT update request failed: %v", err)
	}
	var resp natsClaimUpdateResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return fmt.Errorf("invalid account JWT update response: %v", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("the NATS server refused the account JWT update (%d): %s",
			resp.Error.Code, resp.Error.Description)
	}
	return nil
}
