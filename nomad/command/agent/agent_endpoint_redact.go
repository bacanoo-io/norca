// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/url"
	"strings"
)

// redactedValue replaces a secret in the agent self response.
const redactedValue = "<redacted>"

// redactNorcaConfig removes the secrets of the Norca configuration blocks
// from a copy of the agent configuration before the agent self endpoint
// returns it, like the Vault, Consul and replication tokens above it.
func redactNorcaConfig(c *Config) {
	if c == nil {
		return
	}

	if c.Client != nil {
		if c.Client.ConsulCompat != nil && c.Client.ConsulCompat.AnonymousToken != "" {
			c.Client.ConsulCompat.AnonymousToken = redactedValue
		}
		if c.Client.DNS != nil && c.Client.DNS.Token != "" {
			c.Client.DNS.Token = redactedValue
		}
	}

	if c.Server != nil && c.Server.Admission != nil {
		for _, w := range c.Server.Admission.Webhooks {
			if w != nil {
				w.URL = redactURL(w.URL)
			}
		}
	}

	if c.Server != nil && c.Server.Autoscaler != nil {
		c.Server.Autoscaler.APMs = redactAutoscalerPlugins(c.Server.Autoscaler.APMs)
		c.Server.Autoscaler.Strategies = redactAutoscalerPlugins(c.Server.Autoscaler.Strategies)
	}
}

// redactAutoscalerPlugins returns new plugin blocks without the values of
// their args and config: plugins take API keys, tokens and passwords there
// under names that can't be told apart from harmless settings, so only the
// name, the driver, the number of args and the config keys are kept.
func redactAutoscalerPlugins(plugins []*AutoscalerPluginConfig) []*AutoscalerPluginConfig {
	if plugins == nil {
		return nil
	}
	out := make([]*AutoscalerPluginConfig, 0, len(plugins))
	for _, p := range plugins {
		if p == nil {
			out = append(out, nil)
			continue
		}
		np := &AutoscalerPluginConfig{Name: p.Name, Driver: p.Driver}
		if p.Args != nil {
			np.Args = make([]string, len(p.Args))
			for i := range p.Args {
				np.Args[i] = redactedValue
			}
		}
		if p.Config != nil {
			np.Config = make(map[string]interface{}, len(p.Config))
			for k := range p.Config {
				np.Config[k] = redactedValue
			}
		}
		out = append(out, np)
	}
	return out
}

// redactURL hides the credentials of a URL (a webhook URL) and keeps its
// scheme, host and path: the password of the user information (or a user
// name without a password, which is how tokens are usually passed), every
// query parameter value (the names are kept) and the fragment. A URL that
// can't be parsed is redacted completely.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redactedValue
	}

	changed := false
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		} else {
			u.User = url.User("xxxxx")
		}
		changed = true
	}
	if u.RawQuery != "" {
		params := strings.Split(u.RawQuery, "&")
		for i, param := range params {
			name, value, found := strings.Cut(param, "=")
			switch {
			case found && value != "":
				params[i] = name + "=" + redactedValue
			case !found && param != "":
				// A parameter without "=" may be a bare token.
				params[i] = redactedValue
			}
		}
		u.RawQuery = strings.Join(params, "&")
		changed = true
	}
	fragment := u.Fragment != "" || u.RawFragment != ""
	u.Fragment, u.RawFragment = "", ""

	if !changed && !fragment {
		return raw
	}
	s := u.String()
	if fragment {
		s += "#" + redactedValue
	}
	return s
}
