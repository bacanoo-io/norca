// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package testutil

// TestServer is a test helper. It uses a fork/exec model to create
// a test Nomad server instance in the background and initialize it
// with some data and/or services. The test server can then be used
// to run a unit test, and offers an easy API to tear itself down
// when the test has completed. The only prerequisite is to have a nomad
// binary available on the $PATH.
//
// This package does not use Nomad's official API client. This is
// because we use TestServer to test the API client, which would
// otherwise cause an import cycle.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/nomad/api/internal/testutil/discover"
	testing "github.com/mitchellh/go-testing-interface"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestServerConfig is the main server configuration struct.
type TestServerConfig struct {
	NodeName          string        `json:"name,omitempty"`
	DataDir           string        `json:"data_dir,omitempty"`
	Region            string        `json:"region,omitempty"`
	DisableCheckpoint bool          `json:"disable_update_check"`
	LogLevel          string        `json:"log_level,omitempty"`
	Consul            *Consul       `json:"consul,omitempty"`
	AdvertiseAddrs    *Advertise    `json:"advertise,omitempty"`
	Ports             *PortsConfig  `json:"ports,omitempty"`
	Server            *ServerConfig `json:"server,omitempty"`
	Client            *ClientConfig `json:"client,omitempty"`
	Vault             *VaultConfig  `json:"vault,omitempty"`
	ACL               *ACLConfig    `json:"acl,omitempty"`
	Telemetry         *Telemetry    `json:"telemetry,omitempty"`
	DevMode           bool          `json:"-"`
	Stdout, Stderr    io.Writer     `json:"-"`
}

// Consul is used to configure the communication with Consul
type Consul struct {
	Address string `json:"address,omitempty"`
	Auth    string `json:"auth,omitempty"`
	Token   string `json:"token,omitempty"`
}

// Advertise is used to configure the addresses to advertise
type Advertise struct {
	HTTP string `json:"http,omitempty"`
	RPC  string `json:"rpc,omitempty"`
	Serf string `json:"serf,omitempty"`
}

// PortsConfig is used to configure the network ports we use.
type PortsConfig struct {
	HTTP int `json:"http,omitempty"`
	RPC  int `json:"rpc,omitempty"`
	Serf int `json:"serf,omitempty"`

	// Bus and BusCluster are the ports of the system bus of the server:
	// parallel test agents need their own.
	Bus        int `json:"bus,omitempty"`
	BusCluster int `json:"bus_cluster,omitempty"`
}

// ServerConfig is used to configure the nomad server.
type ServerConfig struct {
	Enabled         bool `json:"enabled"`
	BootstrapExpect int  `json:"bootstrap_expect"`
	RaftProtocol    int  `json:"raft_protocol,omitempty"`
}

// ClientConfig is used to configure the client
type ClientConfig struct {
	Enabled bool              `json:"enabled"`
	Options map[string]string `json:"options,omitempty"`
}

// VaultConfig is used to configure Vault
type VaultConfig struct {
	Enabled bool `json:"enabled"`
}

// ACLConfig is used to configure ACLs
type ACLConfig struct {
	Enabled bool `json:"enabled"`
}

// Telemetry is used to configure the Nomad telemetry setup.
type Telemetry struct {
	PrometheusMetrics bool `json:"prometheus_metrics"`
}

// ServerConfigCallback is a function interface which can be
// passed to NewTestServerConfig to modify the server config.
type ServerConfigCallback func(c *TestServerConfig)

// defaultServerConfig returns a new TestServerConfig struct pre-populated with
// usable config for running as server.
func defaultServerConfig(t testing.T) *TestServerConfig {
	ports := PortAllocator.Grab(5)

	logLevel := "ERROR"
	if envLogLevel := os.Getenv("NOMAD_TEST_LOG_LEVEL"); envLogLevel != "" {
		logLevel = envLogLevel
	}

	return &TestServerConfig{
		NodeName:          fmt.Sprintf("node-%d", ports[0]),
		DisableCheckpoint: true,
		LogLevel:          logLevel,
		Ports: &PortsConfig{
			HTTP:       ports[0],
			RPC:        ports[1],
			Serf:       ports[2],
			Bus:        ports[3],
			BusCluster: ports[4],
		},
		Server: &ServerConfig{
			Enabled:         true,
			BootstrapExpect: 1,
		},
		Client: &ClientConfig{
			Enabled: false,
		},
		Vault: &VaultConfig{
			Enabled: false,
		},
		ACL: &ACLConfig{
			Enabled: false,
		},
	}
}

// TestServer is the main server wrapper struct.
type TestServer struct {
	cmd    *exec.Cmd
	exited chan struct{} // closed when cmd has exited
	Config *TestServerConfig
	t      testing.T

	HTTPAddr   string
	SerfAddr   string
	HTTPClient *http.Client
}

// NewTestServer creates a new TestServer, and makes a call to
// an optional callback function to modify the configuration.
func NewTestServer(t testing.T, cb ServerConfigCallback) *TestServer {
	path, err := discover.NomadExecutable()
	if err != nil {
		t.Skipf("nomad not found, skipping: %v", err)
	}

	// Check that we are actually running nomad
	_, err = exec.Command(path, "-version").CombinedOutput()
	must.NoError(t, err)

	nomadConfig := defaultServerConfig(t)

	if cb != nil {
		cb(nomadConfig)
	}

	if nomadConfig.DevMode {
		if nomadConfig.Client.Options == nil {
			nomadConfig.Client.Options = map[string]string{}
		}
		nomadConfig.Client.Options["test.tighten_network_timeouts"] = "true"
	}

	// The ports were free when they were grabbed, but the agent of another
	// test can bind one of them first. The agent then exits during its
	// startup and is started again with other ports and a new data dir.
	const attempts = 3
	for attempt := 1; ; attempt++ {
		server := startTestServer(t, path, nomadConfig)
		err := server.waitForStart()
		if err == nil {
			// Wait for the client to be ready
			if nomadConfig.DevMode {
				server.waitForClient()
			}
			return server
		}
		exited := server.hasExited()
		server.Stop()
		if !exited || attempt == attempts {
			t.Fatalf("failed to start the agent: %v", err)
		}
		t.Logf("the agent exited during its startup (attempt %d), starting it with other ports: %v", attempt, err)
		ports := PortAllocator.Grab(5)
		nomadConfig.Ports = &PortsConfig{HTTP: ports[0], RPC: ports[1], Serf: ports[2], Bus: ports[3], BusCluster: ports[4]}
	}
}

// startTestServer starts the agent with the configuration in a new data dir.
func startTestServer(t testing.T, path string, nomadConfig *TestServerConfig) *TestServer {
	dataDir, err := os.MkdirTemp("", "nomad")
	must.NoError(t, err)
	nomadConfig.DataDir = dataDir

	configFile, err := os.CreateTemp(dataDir, "nomad")
	must.NoError(t, err)

	configContent, err := json.Marshal(nomadConfig)
	must.NoError(t, err)

	_, err = configFile.Write(configContent)
	must.NoError(t, err)
	must.NoError(t, configFile.Sync())
	must.NoError(t, configFile.Close())

	args := []string{"agent", "-config", configFile.Name()}
	if nomadConfig.DevMode {
		args = append(args, "-dev")
	}

	stdout := io.Writer(os.Stdout)
	if nomadConfig.Stdout != nil {
		stdout = nomadConfig.Stdout
	}

	stderr := io.Writer(os.Stderr)
	if nomadConfig.Stderr != nil {
		stderr = nomadConfig.Stderr
	}

	// Start the server
	cmd := exec.Command(path, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	must.NoError(t, cmd.Start())

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()

	client := cleanhttp.DefaultClient()
	client.Timeout = 10 * time.Second

	return &TestServer{
		Config: nomadConfig,
		cmd:    cmd,
		exited: exited,
		t:      t,

		HTTPAddr:   fmt.Sprintf("127.0.0.1:%d", nomadConfig.Ports.HTTP),
		SerfAddr:   fmt.Sprintf("127.0.0.1:%d", nomadConfig.Ports.Serf),
		HTTPClient: client,
	}
}

// waitForStart waits for the server to be ready: for a server that
// bootstraps, until it has an established leader, otherwise until its HTTP
// API responds.
func (s *TestServer) waitForStart() error {
	if s.Config.Server.Enabled && s.Config.Server.BootstrapExpect != 0 {
		if err := s.waitForLeader(); err != nil {
			return err
		}
		return s.waitForEstablished("")
	}
	return s.waitForAPI()
}

// hasExited reports whether the agent process has exited.
func (s *TestServer) hasExited() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// Stop stops the test Nomad server, and removes the Nomad data
// directory once we are done.
func (s *TestServer) Stop() {
	defer func() { _ = os.RemoveAll(s.Config.DataDir) }()

	// wait for the process to exit to be sure that the data dir can be
	// deleted on all platforms.
	if s.hasExited() {
		return
	}

	// kill and wait gracefully
	err := s.cmd.Process.Signal(os.Interrupt)
	must.NoError(s.t, err)

	select {
	case <-s.exited:
		return
	case <-time.After(5 * time.Second):
		s.t.Logf("timed out waiting for process to gracefully terminate")
	}

	err = s.cmd.Process.Kill()
	must.NoError(s.t, err, must.Sprint("failed to kill process"))

	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		s.t.Logf("timed out waiting for process to be killed")
	}
}

// waitFor calls f until it succeeds, the timeout passes, or the agent
// exits, and returns the last error of f.
func (s *TestServer) waitFor(timeout, gap time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return nil
		}
		select {
		case <-s.exited:
			return fmt.Errorf("agent exited: %w", err)
		case <-time.After(gap):
		}
		if time.Now().After(deadline) {
			return err
		}
	}
}

// waitForAPI waits for only the agent HTTP endpoint to start
// responding. This is an indication that the agent has started,
// but will likely return before a leader is elected.
func (s *TestServer) waitForAPI() error {
	f := func() error {
		resp, err := s.HTTPClient.Get(s.url("/v1/metrics"))
		if err != nil {
			return fmt.Errorf("failed to get metrics: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err = s.requireOK(resp); err != nil {
			return fmt.Errorf("metrics response is not ok: %w", err)
		}
		return nil
	}
	if err := s.waitFor(20*time.Second, 250*time.Millisecond, f); err != nil {
		return fmt.Errorf("failed to wait for api: %w", err)
	}
	return nil
}

// waitForLeader waits for the Nomad server's HTTP API to become
// available, and then waits for a known leader to be observed.
// WaitForLeaderEstablished waits for the leader's establishment.
func (s *TestServer) waitForLeader() error {
	f := func() error {
		// Query the API and check the status code
		// Using this endpoint as it is does not have restricted access
		resp, err := s.HTTPClient.Get(s.url("/v1/status/leader"))
		if err != nil {
			return fmt.Errorf("failed to get leader: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err = s.requireOK(resp); err != nil {
			return fmt.Errorf("leader response is not ok: %w", err)
		}
		return nil
	}
	if err := s.waitFor(20*time.Second, 250*time.Millisecond, f); err != nil {
		return fmt.Errorf("failed to wait for leader: %w", err)
	}
	return nil
}

// WaitForLeaderEstablished waits until the leader has established its
// leadership as far as the tests depend on it. A leader is reported as soon
// as it is elected, but it writes the scheduler configuration (job
// registrations are refused without one) and the first root key (variables
// can't be encrypted without one) only afterwards.
//
// Both are read with the operator API, which needs a management token when
// ACLs are enabled. Without a token the wait then ends at the first
// permission denied; tests of an ACL-enabled server wait again with the
// bootstrap token (see makeACLClient).
func (s *TestServer) WaitForLeaderEstablished(token string) {
	if err := s.waitForEstablished(token); err != nil {
		s.Stop()
		s.t.Fatalf("%v", err)
	}
}

func (s *TestServer) waitForEstablished(token string) error {
	f := func() error {
		var sched struct {
			SchedulerConfig *struct{}
		}
		switch err := s.getJSON("/v1/operator/scheduler/configuration", token, &sched); {
		case errors.Is(err, errPermissionDenied):
			return nil
		case err != nil:
			return fmt.Errorf("failed to get the scheduler configuration: %w", err)
		case sched.SchedulerConfig == nil:
			return errors.New("no scheduler configuration yet")
		}

		var keys []struct {
			KeyID string
			State string
		}
		switch err := s.getJSON("/v1/operator/keyring/keys", token, &keys); {
		case errors.Is(err, errPermissionDenied):
			return nil
		case err != nil:
			return fmt.Errorf("failed to list the root keys: %w", err)
		}
		for _, key := range keys {
			if key.State == "active" {
				return nil
			}
		}
		return errors.New("no active root key yet")
	}
	if err := s.waitFor(30*time.Second, 100*time.Millisecond, f); err != nil {
		return fmt.Errorf("failed to wait for the leader to be established: %w", err)
	}
	return nil
}

// errPermissionDenied is returned by getJSON for a 403 response.
var errPermissionDenied = errors.New("permission denied")

// getJSON performs an HTTP GET request with the token (if not empty) and
// decodes the JSON response into out.
func (s *TestServer) getJSON(path, token string, out any) error {
	req, err := http.NewRequest(http.MethodGet, s.url(path), nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Nomad-Token", token)
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		return errPermissionDenied
	}
	if err = s.requireOK(resp); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// waitForClient waits for the Nomad client to be ready. The function returns
// immediately if the server is not in dev mode.
func (s *TestServer) waitForClient() {
	if !s.Config.DevMode {
		return
	}
	f := func() error {
		resp, err := s.HTTPClient.Get(s.url("/v1/nodes"))
		if err != nil {
			return fmt.Errorf("failed to get nodes: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err = s.requireOK(resp); err != nil {
			return fmt.Errorf("nodes response not ok: %w", err)
		}
		var decoded []struct {
			ID     string
			Status string
		}
		if err = json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return fmt.Errorf("failed to decode nodes response: %w", err)
		}
		return nil
	}
	must.Wait(s.t,
		wait.InitialSuccess(
			wait.ErrorFunc(f),
			wait.Timeout(10*time.Second),
			wait.Gap(1*time.Second),
		),
		must.Sprint("failed to wait for client (node)"),
	)
}

// url is a helper function which takes a relative URL and
// makes it into a proper URL against the local Nomad server.
func (s *TestServer) url(path string) string {
	return fmt.Sprintf("http://%s%s", s.HTTPAddr, path)
}

// requireOK checks the HTTP response code and ensures it is acceptable.
func (s *TestServer) requireOK(resp *http.Response) error {
	if resp.StatusCode != 200 {
		return fmt.Errorf("bad status code: %d", resp.StatusCode)
	}
	return nil
}

// put performs a new HTTP PUT request.
func (s *TestServer) put(path string, body io.Reader) *http.Response {
	req, err := http.NewRequest("PUT", s.url(path), body)
	must.NoError(s.t, err)

	resp, err := s.HTTPClient.Do(req)
	must.NoError(s.t, err)

	if err = s.requireOK(resp); err != nil {
		_ = resp.Body.Close()
		must.NoError(s.t, err)
	}
	return resp
}

// get performs a new HTTP GET request.
func (s *TestServer) get(path string) *http.Response {
	resp, err := s.HTTPClient.Get(s.url(path))
	must.NoError(s.t, err)

	if err = s.requireOK(resp); err != nil {
		_ = resp.Body.Close()
		must.NoError(s.t, err)
	}
	return resp
}

// encodePayload returns a new io.Reader wrapping the encoded contents
// of the payload, suitable for passing directly to a new request.
func (s *TestServer) encodePayload(payload any) io.Reader {
	var encoded bytes.Buffer
	err := json.NewEncoder(&encoded).Encode(payload)
	must.NoError(s.t, err)
	return &encoded
}
