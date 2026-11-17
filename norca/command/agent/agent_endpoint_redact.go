// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/url"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/structs/config"
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
		if cc := c.Client.ConsulCompat; cc != nil && cc.KV != nil && cc.KV.NATS != nil {
			if cc.KV.NATS.Password != "" {
				cc.KV.NATS.Password = redactedValue
			}
			if cc.KV.NATS.Token != "" {
				cc.KV.NATS.Token = redactedValue
			}
			// nats://user:password@host and nats://token@host
			cc.KV.NATS.URLs = redactAddresses(cc.KV.NATS.URLs)
		}
		if c.Client.DNS != nil && c.Client.DNS.Token != "" {
			c.Client.DNS.Token = redactedValue
		}
		c.Client.Servers = redactJoinAddresses(c.Client.Servers)
		redactServerJoin(c.Client.ServerJoin)
		c.Client.Options = redactOptions(c.Client.Options)
	}

	if c.Server != nil {
		c.Server.StartJoin = redactJoinAddresses(c.Server.StartJoin)
		c.Server.RetryJoin = redactJoinAddresses(c.Server.RetryJoin)
		redactServerJoin(c.Server.ServerJoin)
		if c.Server.Backup != nil {
			for _, d := range c.Server.Backup.Destinations {
				if d != nil {
					d.Endpoint = redactAddress(d.Endpoint)
				}
			}
		}
	}

	if c.Consul != nil {
		// auth is the "user:password" of HTTP basic authentication
		if c.Consul.Auth != "" {
			c.Consul.Auth = redactedValue
		}
		c.Consul.Addr = redactAddress(c.Consul.Addr)
		c.Consul.GRPCAddr = redactAddress(c.Consul.GRPCAddr)
	}
	if c.Vault != nil {
		c.Vault.Addr = redactAddress(c.Vault.Addr)
	}
	if c.Telemetry != nil {
		c.Telemetry.CirconusAPIURL = redactAddress(c.Telemetry.CirconusAPIURL)
		// the submission URL of a check ends in its secret
		if c.Telemetry.CirconusCheckSubmissionURL != "" {
			c.Telemetry.CirconusCheckSubmissionURL = redactedValue
		}
	}
	c.Plugins = redactPlugins(c.Plugins)

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
// scheme, host and path: the whole user information (tokens are passed as
// the user name, with or without a password: "token@", "token:@",
// "token:x-oauth-basic@"), every query parameter value (the names are kept)
// and the fragment. A URL that can't be parsed is redacted completely.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redactedValue
	}

	changed := false
	if u.User != nil {
		u.User = url.User("xxxxx")
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

// redactAddress hides the credentials of a setting that is a URL or a bare
// "host:port" address, which a client library completes with its scheme
// ("user:password@host:4222" is a NATS server too). An address without
// credentials is returned as it is.
func redactAddress(raw string) string {
	if raw == "" || strings.Contains(raw, "://") {
		return redactURL(raw)
	}
	const scheme = "x://"
	out := redactURL(scheme + raw)
	if out == redactedValue {
		if strings.ContainsAny(raw, "@?#") {
			return redactedValue
		}
		return raw
	}
	return strings.TrimPrefix(out, scheme)
}

// redactAddresses returns a new list with the credentials of every address
// hidden. An element may itself be a comma separated list.
func redactAddresses(addrs []string) []string {
	if addrs == nil {
		return nil
	}
	out := make([]string, len(addrs))
	for i, addr := range addrs {
		parts := strings.Split(addr, ",")
		for j, part := range parts {
			parts[j] = redactAddress(strings.TrimSpace(part))
		}
		out[i] = strings.Join(parts, ",")
	}
	return out
}

// redactJoinAddresses returns a new list of the addresses of servers to
// join. A cloud auto-join entry ("provider=aws access_key_id=...
// secret_access_key=...") keeps its provider and the names of its settings:
// which of them are credentials depends on the provider.
func redactJoinAddresses(addrs []string) []string {
	if addrs == nil {
		return nil
	}
	out := make([]string, len(addrs))
	for i, addr := range addrs {
		if !strings.Contains(addr, "provider=") {
			out[i] = redactAddress(addr)
			continue
		}
		fields := strings.Fields(addr)
		for j, field := range fields {
			name, _, found := strings.Cut(field, "=")
			switch {
			case !found:
				fields[j] = redactedValue
			case name != "provider":
				fields[j] = name + "=" + redactedValue
			}
		}
		out[i] = strings.Join(fields, " ")
	}
	return out
}

// redactServerJoin hides the credentials of the join addresses of a
// server_join block.
func redactServerJoin(j *ServerJoin) {
	if j == nil {
		return
	}
	j.StartJoin = redactJoinAddresses(j.StartJoin)
	j.RetryJoin = redactJoinAddresses(j.RetryJoin)
}

// secretNameParts are the parts of the name of a setting that mark its
// value as a credential in the free-form settings of plugins and client
// options.
var secretNameParts = []string{
	"token", "secret", "password", "passwd", "passphrase", "credential",
	"auth", "apikey", "api_key", "api-key", "private_key", "private-key",
	"access_key", "access-key", "signature", "cookie", "session",
}

// isSecretName reports whether the name of a free-form setting says that
// its value is a credential.
func isSecretName(name string) bool {
	name = strings.ToLower(name)
	for _, part := range secretNameParts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// redactFreeForm returns a copy of a value of a free-form setting: the
// values under names that look like credentials are replaced, and so are
// the credentials of URLs. It is a best effort, as nothing says what the
// settings of a plugin mean: a credential under a harmless name stays.
func redactFreeForm(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "://") {
			return redactAddress(t)
		}
		return t
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			if isSecretName(k) {
				out[k] = redactedValue
			} else {
				out[k] = redactFreeForm(e)
			}
		}
		return out
	case []map[string]interface{}:
		out := make([]map[string]interface{}, len(t))
		for i, e := range t {
			out[i], _ = redactFreeForm(e).(map[string]interface{})
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = redactFreeForm(e)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, e := range t {
			out[i], _ = redactFreeForm(e).(string)
		}
		return out
	}
	return v
}

// redactArgs returns a copy of the arguments of a plugin with the same best
// effort: "-token=x" and "--password x" lose their value, URLs their
// credentials.
func redactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	secretNext := false
	for i, arg := range args {
		name, value, found := strings.Cut(arg, "=")
		switch {
		case secretNext:
			out[i] = redactedValue
		case found && isSecretName(name):
			out[i] = name + "=" + redactedValue
		case found:
			value, _ = redactFreeForm(value).(string)
			out[i] = name + "=" + value
		default:
			out[i], _ = redactFreeForm(arg).(string)
		}
		secretNext = !secretNext && !found && strings.HasPrefix(arg, "-") && isSecretName(arg)
	}
	return out
}

// redactPlugins returns new plugin blocks (task drivers and devices) with
// the credentials of their args and config hidden.
func redactPlugins(plugins []*config.PluginConfig) []*config.PluginConfig {
	if plugins == nil {
		return nil
	}
	out := make([]*config.PluginConfig, 0, len(plugins))
	for _, p := range plugins {
		if p == nil {
			out = append(out, nil)
			continue
		}
		np := &config.PluginConfig{Name: p.Name, Args: redactArgs(p.Args)}
		if p.Config != nil {
			np.Config, _ = redactFreeForm(p.Config).(map[string]interface{})
		}
		out = append(out, np)
	}
	return out
}

// redactOptions returns a copy of the options of the client with the same
// best effort.
func redactOptions(options map[string]string) map[string]string {
	if options == nil {
		return nil
	}
	out := make(map[string]string, len(options))
	for k, v := range options {
		if isSecretName(k) {
			out[k] = redactedValue
		} else {
			out[k], _ = redactFreeForm(v).(string)
		}
	}
	return out
}
