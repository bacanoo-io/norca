package ldapauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// realConnector is the production [Connector], adapting go-ldap/ldap/v3. It is
// deliberately thin: all decision logic lives in the method, so this file is the
// only place the LDAP library is used (ADR D-018). It is exercised against a
// real directory, not in unit tests.
type realConnector struct{}

func (realConnector) Authenticate(_ context.Context, cfg *Config, username, password string) ([]string, error) {
	l, err := dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("ldapauth: connect: %w", err)
	}
	defer func() { _ = l.Close() }()

	// Bind as the service account (or anonymously) to search for the user.
	if cfg.BindDN != "" {
		if err := l.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
			return nil, fmt.Errorf("ldapauth: service bind: %w", err)
		}
	}

	userAttr := cfg.UserAttr
	if userAttr == "" {
		userAttr = "cn"
	}
	filter := fmt.Sprintf("(%s=%s)", userAttr, ldap.EscapeFilter(username))
	sr, err := l.Search(ldap.NewSearchRequest(
		cfg.UserDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
		filter, []string{"dn"}, nil,
	))
	if err != nil {
		return nil, fmt.Errorf("ldapauth: user search: %w", err)
	}
	if len(sr.Entries) != 1 {
		return nil, ErrDenied // no such user, or ambiguous
	}
	userDN := sr.Entries[0].DN

	// Verify the password by binding as the user.
	if err := l.Bind(userDN, password); err != nil {
		return nil, ErrDenied
	}

	if cfg.GroupDN == "" {
		return nil, nil // groups not configured
	}
	// Re-bind as the service account for the group search (the user may lack
	// search rights); if anonymous, stay bound as the user.
	if cfg.BindDN != "" {
		if err := l.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
			return nil, fmt.Errorf("ldapauth: service rebind: %w", err)
		}
	}
	return searchGroups(l, cfg, userDN, username)
}

func searchGroups(l *ldap.Conn, cfg *Config, userDN, username string) ([]string, error) {
	groupAttr := cfg.GroupAttr
	if groupAttr == "" {
		groupAttr = "cn"
	}
	tmpl := cfg.GroupFilter
	if tmpl == "" {
		tmpl = "(member={{.UserDN}})"
	}
	filter := strings.NewReplacer(
		"{{.UserDN}}", ldap.EscapeFilter(userDN),
		"{{.Username}}", ldap.EscapeFilter(username),
	).Replace(tmpl)

	sr, err := l.Search(ldap.NewSearchRequest(
		cfg.GroupDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter, []string{groupAttr}, nil,
	))
	if err != nil {
		return nil, fmt.Errorf("ldapauth: group search: %w", err)
	}
	var groups []string
	for _, e := range sr.Entries {
		for _, v := range e.GetAttributeValues(groupAttr) {
			if v != "" {
				groups = append(groups, v)
			}
		}
	}
	return groups, nil
}

// dial opens a connection to the directory, honoring ldaps:// and StartTLS.
func dial(cfg *Config) (*ldap.Conn, error) {
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	l, err := ldap.DialURL(cfg.URL, ldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, err
	}
	if cfg.StartTLS {
		if err := l.StartTLS(tlsCfg); err != nil {
			_ = l.Close()
			return nil, err
		}
	}
	return l, nil
}

// tlsConfig is the TLS configuration for talking to the directory: verified
// against cfg.Certificate when set (and only against it), else the system roots,
// unless insecure_tls turns verification off.
func tlsConfig(cfg *Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureTLS} //nolint:gosec // G402: opt-in dev-only skip, documented as insecure_tls
	if host := hostFromURL(cfg.URL); host != "" {
		tlsCfg.ServerName = host
	}
	if cfg.Certificate != "" {
		pool, err := certPool(cfg.Certificate)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}

// certPool parses a PEM bundle that must contain one or more certificates and
// nothing else (a private key pasted by mistake is refused, not stored).
func certPool(bundle string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	rest := []byte(bundle)
	n := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, ErrInvalidCertificate
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, ErrInvalidCertificate
		}
		pool.AddCert(c)
		n++
	}
	if n == 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrInvalidCertificate
	}
	return pool, nil
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ensure realConnector satisfies Connector.
var _ Connector = realConnector{}
