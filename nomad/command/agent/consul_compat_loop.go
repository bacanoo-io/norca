// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net"
	"strings"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/pointer"
)

// consulFingerprintName is the name of the Consul fingerprinter of the client.
const consulFingerprintName = "consul"

// consulTargetsConsulCompat returns whether the agent's own Consul client
// (the consul block) would talk to the Consul-compatible API of this agent's
// client (client.consul_compat): both default to 127.0.0.1:8500.
func (c *Config) consulTargetsConsulCompat() bool {
	if c == nil || c.Consul == nil || c.Client == nil || !c.Client.Enabled || !c.Client.ConsulCompat.IsEnabled() {
		return false
	}
	apiConf, err := c.Consul.ApiConfig()
	if err != nil {
		return false
	}
	return addrTargetsListener(apiConf.Address, c.Client.ConsulCompat.BindAddr())
}

// addrTargetsListener returns whether a client dialing addr (a Consul API
// address: host:port, optionally with an http:// or https:// scheme) reaches
// a listener bound to bind on this host. Host names other than localhost are
// not resolved: they match only when they are written the same way.
func addrTargetsListener(addr, bind string) bool {
	if scheme, rest, ok := strings.Cut(addr, "://"); ok {
		if scheme != "http" && scheme != "https" {
			// unix sockets and other transports never reach a TCP listener
			return false
		}
		addr = rest
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	bindHost, bindPort, err := net.SplitHostPort(bind)
	if err != nil || port != bindPort {
		return false
	}
	if strings.EqualFold(host, bindHost) {
		return true
	}

	switch {
	case isWildcardHost(bindHost):
		// the listener accepts connections on every local address
		return isLoopbackHost(host) || isWildcardHost(host) || isLocalIP(host)
	case strings.EqualFold(host, "localhost"):
		return isLoopbackHost(bindHost)
	case isWildcardHost(host):
		// dialing the unspecified address connects to the local system
		return isLoopbackHost(bindHost)
	}
	hostIP, bindIP := net.ParseIP(host), net.ParseIP(bindHost)
	return hostIP != nil && bindIP != nil && hostIP.Equal(bindIP)
}

// isWildcardHost returns whether host is the unspecified address.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// isLoopbackHost returns whether host is localhost or a loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLocalIP returns whether host is an IP address of a local interface.
func isLocalIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// avoidConsulCompatLoop keeps the agent's own Consul integration from
// talking to the agent itself when the consul block points at the
// Consul-compatible API of the client (both default to 127.0.0.1:8500).
// Without it the agent would register itself into its own native catalog
// through the Consul agent API (and fail on the check endpoints, which the
// API does not serve, retrying forever), join its servers through its own
// catalog, and fingerprint itself as a Consul agent. It turns off
// auto_advertise, server_auto_join and client_auto_join and the Consul
// fingerprinter, and logs what it changed. Templates keep reading services
// through the consul block's address, which the Consul-compatible API serves.
// Pointing consul.address or consul_compat.bind elsewhere restores the
// integration.
func avoidConsulCompatLoop(c *Config, logger log.Logger) {
	if !c.consulTargetsConsulCompat() {
		return
	}

	var changed []string
	for _, setting := range []struct {
		name string
		val  **bool
	}{
		{"auto_advertise", &c.Consul.AutoAdvertise},
		{"server_auto_join", &c.Consul.ServerAutoJoin},
		{"client_auto_join", &c.Consul.ClientAutoJoin},
	} {
		if *setting.val == nil || **setting.val {
			changed = append(changed, "consul."+setting.name)
			*setting.val = pointer.Of(false)
		}
	}

	options := make(map[string]string, len(c.Client.Options)+1)
	for k, v := range c.Client.Options {
		options[k] = v
	}
	denylist := options["fingerprint.denylist"]
	if denylist == "" {
		// the deprecated name, read when the new one is not set
		denylist = options["fingerprint.blacklist"]
	}
	if _, ok := splitList(denylist)[consulFingerprintName]; !ok {
		if denylist != "" {
			denylist += ","
		}
		options["fingerprint.denylist"] = denylist + consulFingerprintName
		changed = append(changed, "the consul fingerprinter")
	}
	c.Client.Options = options

	if len(changed) == 0 {
		return
	}
	logger.Info("the consul block points at the Consul-compatible API of this agent; "+
		"turned off the parts of the Consul integration that would talk to the agent itself "+
		"(set consul.address or client.consul_compat.bind to another address to keep them)",
		"consul_address", c.Consul.Addr, "consul_compat_bind", c.Client.ConsulCompat.BindAddr(),
		"turned_off", strings.Join(changed, ", "))
}

// splitList returns the set of the comma separated values of list.
func splitList(list string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, v := range strings.Split(list, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

// consulCompatLoopWarning returns the warning config validate prints when the
// agent will turn off parts of its Consul integration, empty otherwise.
func consulCompatLoopWarning(c *Config) string {
	if !c.consulTargetsConsulCompat() {
		return ""
	}
	return "consul.address points at client.consul_compat (" + c.Client.ConsulCompat.BindAddr() +
		"): so that the agent does not talk to itself, it runs with consul.auto_advertise, " +
		"consul.server_auto_join and consul.client_auto_join off and without the consul fingerprinter; " +
		"templates still read services through it"
}
