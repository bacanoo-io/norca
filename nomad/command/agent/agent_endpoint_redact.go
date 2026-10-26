// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import "net/url"

// redactNorcaConfig removes the secrets of the Norca configuration blocks
// from a copy of the agent configuration before the agent self endpoint
// returns it, like the Vault, Consul and replication tokens above it.
func redactNorcaConfig(c *Config) {
	if c == nil {
		return
	}

	if c.Client != nil {
		if c.Client.ConsulCompat != nil && c.Client.ConsulCompat.AnonymousToken != "" {
			c.Client.ConsulCompat.AnonymousToken = "<redacted>"
		}
		if c.Client.DNS != nil && c.Client.DNS.Token != "" {
			c.Client.DNS.Token = "<redacted>"
		}
	}

	if c.Server != nil && c.Server.Admission != nil {
		for _, w := range c.Server.Admission.Webhooks {
			if w != nil {
				w.URL = redactURLPassword(w.URL)
			}
		}
	}
}

// redactURLPassword hides the password of a URL with user information (a
// webhook URL with basic auth credentials); other URLs are returned as is.
func redactURLPassword(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); !ok {
		return raw
	}
	return u.Redacted()
}
