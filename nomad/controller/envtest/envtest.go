// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package envtest runs a real Norca dev agent for the tests of controllers,
// so that they run against the real API (schemas, CEL rules, finalizers,
// the event stream) instead of a fake. The SDK depends only on the API
// module, so the agent runs as a separate process from the norca binary
// named by NORCA_BINARY, or found on the PATH.
//
//	func TestReconcile(t *testing.T) {
//		agent := envtest.StartT(t, envtest.Options{})   // skipped without a binary
//		client := agent.Client
//		...
//	}
//
// The agent listens on loopback ports only, keeps its data and log in a
// temporary directory that is removed when it stops, and is killed after
// Options.MaxLifetime at the latest, so a test that hangs never leaves it
// behind. With the controller SDK's fake backend (package fake) tests need
// no agent at all; envtest is for the tests that need the real servers.
package envtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

// Options configure the agent.
type Options struct {
	// Binary is the norca binary; default: NORCA_BINARY, or norca (or
	// nomad) on the PATH.
	Binary string

	// EventBufferSize sets server.event_buffer_size (0: the default).
	EventBufferSize int

	// ExtraConfig is appended to the agent configuration (HCL).
	ExtraConfig string

	// ACL enables ACLs and bootstraps them: Agent.Client and Agent.Env
	// carry the management token (Agent.Token).
	ACL bool

	// StartTimeout bounds the wait for an established leader (default 90s).
	StartTimeout time.Duration

	// MaxLifetime kills the agent after this time (default 30m).
	MaxLifetime time.Duration

	// KeepLog keeps the log file after Stop (for debugging); its path is
	// Agent.LogFile.
	KeepLog bool
}

// Agent is a running dev agent.
type Agent struct {
	// Client is an API client for the agent.
	Client *api.Client

	// Address is the HTTP address ("http://127.0.0.1:<port>").
	Address string

	// LogFile is the path of the agent's log.
	LogFile string

	// Token is the bootstrap (management) token with Options.ACL.
	Token string

	dir     string
	cmd     *exec.Cmd
	exited  chan struct{}
	log     *os.File
	killer  *time.Timer
	stopped sync.Once
	keepLog bool
}

// ErrNoBinary is returned by FindBinary when no norca binary is found.
var ErrNoBinary = errors.New("no norca binary: set NORCA_BINARY or put norca on the PATH")

// FindBinary returns the norca binary to run.
func FindBinary() (string, error) {
	if bin := os.Getenv("NORCA_BINARY"); bin != "" {
		return bin, nil
	}
	for _, name := range []string{"norca", "nomad"} {
		if bin, err := exec.LookPath(name); err == nil {
			return bin, nil
		}
	}
	return "", ErrNoBinary
}

// FreePort returns a loopback port that is free for TCP and UDP. Ephemeral
// ports are avoided: on Windows many of them are reserved.
func FreePort() (int, error) {
	for range 100 {
		port := 20000 + rand.IntN(12000)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		udp, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		ln.Close()
		if err == nil {
			udp.Close()
			return port, nil
		}
	}
	return 0, errors.New("no free port")
}

// StartT starts an agent for a test. The test is skipped with -short or
// when no binary is found; the agent is stopped when the test ends, and its
// log is printed if the test failed. Starting is retried, as a port found
// free can be taken by the time the agent binds it.
func StartT(t testing.TB, opts Options) *Agent {
	t.Helper()
	if testing.Short() {
		t.Skip("agent test skipped with -short")
	}
	if opts.Binary == "" {
		bin, err := FindBinary()
		if err != nil {
			t.Skip(err.Error())
		}
		opts.Binary = bin
	}
	var agent *Agent
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		opts.KeepLog = true
		agent, err = Start(context.Background(), opts)
		if err == nil {
			break
		}
		t.Logf("starting the agent failed (attempt %d): %v", attempt, err)
	}
	if err != nil {
		t.Fatalf("starting the agent failed: %v", err)
	}
	t.Cleanup(func() {
		agent.Stop()
		if t.Failed() {
			if b, err := os.ReadFile(agent.LogFile); err == nil && len(b) > 0 {
				t.Logf("agent log (tail):\n%s", tail(b, 4000))
			}
		}
		_ = os.RemoveAll(agent.dir)
	})
	return agent
}

// Start starts a dev agent and waits until it has an established leader.
func Start(ctx context.Context, opts Options) (*Agent, error) {
	if opts.Binary == "" {
		bin, err := FindBinary()
		if err != nil {
			return nil, err
		}
		opts.Binary = bin
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 90 * time.Second
	}
	if opts.MaxLifetime <= 0 {
		opts.MaxLifetime = 30 * time.Minute
	}

	dir, err := os.MkdirTemp("", "norca-envtest-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Agent, error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	var ports [3]int
	for i := range ports {
		if ports[i], err = FreePort(); err != nil {
			return fail(err)
		}
	}
	server := ""
	if opts.EventBufferSize > 0 {
		server = fmt.Sprintf("server {\n  event_buffer_size = %d\n}\n", opts.EventBufferSize)
	}
	if opts.ACL {
		server += "acl {\n  enabled = true\n}\n"
	}
	config := fmt.Sprintf(`
bind_addr = "127.0.0.1"
data_dir  = %q
ports {
  http = %d
  rpc  = %d
  serf = %d
}
%s
%s
`, filepath.ToSlash(filepath.Join(dir, "data")), ports[0], ports[1], ports[2], server, opts.ExtraConfig)
	configFile := filepath.Join(dir, "agent.hcl")
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		return fail(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "agent.log"))
	if err != nil {
		return fail(err)
	}

	cmd := exec.Command(opts.Binary, "agent", "-dev", "-config", configFile)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fail(err)
	}
	a := &Agent{
		Address: fmt.Sprintf("http://127.0.0.1:%d", ports[0]),
		LogFile: logFile.Name(),
		dir:     dir,
		cmd:     cmd,
		exited:  make(chan struct{}),
		log:     logFile,
		keepLog: opts.KeepLog,
	}
	go func() {
		_ = cmd.Wait()
		close(a.exited)
	}()
	a.killer = time.AfterFunc(opts.MaxLifetime, func() { _ = cmd.Process.Kill() })

	conf := api.DefaultConfig()
	conf.Address = a.Address
	conf.SecretID = ""
	if a.Client, err = api.NewClient(conf); err != nil {
		a.Stop()
		return nil, err
	}

	deadline := time.Now().Add(opts.StartTimeout)
	for {
		if a.ready(opts.ACL) {
			return a, nil
		}
		select {
		case <-a.exited:
			b, _ := os.ReadFile(a.LogFile)
			a.Stop()
			return nil, fmt.Errorf("agent exited:\n%s", tail(b, 2000))
		case <-ctx.Done():
			a.Stop()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			a.Stop()
			return nil, fmt.Errorf("no established leader within %s", opts.StartTimeout)
		}
	}
}

// ready reports whether the agent has a leader that has established its
// leadership, bootstrapping ACLs first with acl. A leader is reported as
// soon as it is elected, but it writes the scheduler configuration (job
// registrations are refused without one) and the first root key (variables
// can't be encrypted without one) only afterwards.
func (a *Agent) ready(acl bool) bool {
	if a.Token == "" {
		leader, err := a.Client.Status().Leader()
		if err != nil || leader == "" {
			return false
		}
		if acl {
			// The ACL subsystem may not be ready yet; retry.
			token, _, err := a.Client.ACLTokens().Bootstrap(nil)
			if err != nil {
				return false
			}
			a.Token = token.SecretID
			a.Client.SetSecretID(token.SecretID)
		}
	}
	sched, _, err := a.Client.Operator().SchedulerGetConfiguration(nil)
	if err != nil || sched.SchedulerConfig == nil {
		return false
	}
	keys, _, err := a.Client.Keyring().List(nil)
	if err != nil {
		return false
	}
	for _, key := range keys {
		if key.State == api.RootKeyStateActive {
			return true
		}
	}
	return false
}

// Stop kills the agent and removes its directory (unless KeepLog is set).
func (a *Agent) Stop() {
	a.stopped.Do(func() {
		a.killer.Stop()
		_ = a.cmd.Process.Kill()
		<-a.exited
		_ = a.log.Close()
		if !a.keepLog {
			_ = os.RemoveAll(a.dir)
		}
	})
}

// Env returns the environment variables that point the norca CLI at the
// agent (NOMAD_ADDR, NORCA_ADDR, and the token with Options.ACL).
func (a *Agent) Env() []string {
	env := []string{"NOMAD_ADDR=" + a.Address, "NORCA_ADDR=" + a.Address}
	if a.Token != "" {
		env = append(env, "NOMAD_TOKEN="+a.Token, "NORCA_TOKEN="+a.Token)
	}
	return env
}

// Binary returns the binary the agent runs, for running CLI commands
// against it.
func (a *Agent) Binary() string {
	return a.cmd.Path
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
