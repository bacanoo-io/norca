// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/go-hclog"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// auditSink collects the audit records a server logs.
type auditSink struct {
	mu      sync.Mutex
	records []map[string]any
	// text holds every record formatted as a log line would be.
	text strings.Builder
}

func (s *auditSink) Accept(name string, level hclog.Level, msg string, args ...any) {
	if msg != "operator write call" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := map[string]any{"@module": name, "@level": level}
	for i := 0; i+1 < len(args); i += 2 {
		key, _ := args[i].(string)
		rec[key] = args[i+1]
		fmt.Fprintf(&s.text, "%s=%v ", key, args[i+1])
	}
	s.text.WriteString("\n")
	s.records = append(s.records, rec)
}

// take returns the records logged since the last call.
func (s *auditSink) take() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records
	s.records = nil
	return r
}

func (s *auditSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// withAuditSink registers sink on the logger of a test server.
func withAuditSink(sink *auditSink) func(*Config) {
	return func(c *Config) {
		c.Logger.RegisterSink(sink)
	}
}

// auditEndpoints are the receivers of the recorded endpoints, by service.
var auditEndpoints = map[string]any{
	"Operator":    &Operator{},
	"Keyring":     &Keyring{},
	"Backup":      &Backup{},
	"Certificate": &Certificate{},
	"MTLS":        &MTLS{},
	"Node":        &Node{},
	"System":      &System{},
}

// auditRequest returns a new request of the type the endpoint of an
// operation takes.
func auditRequest(t *testing.T, method string) any {
	t.Helper()
	if method == "Operator.SnapshotRestore" {
		return &structs.SnapshotRestoreRequest{}
	}
	service, name, _ := strings.Cut(method, ".")
	recv, ok := auditEndpoints[service]
	must.True(t, ok, must.Sprintf("no endpoint of service %s", service))
	m, ok := reflect.TypeOf(recv).MethodByName(name)
	must.True(t, ok, must.Sprintf("no endpoint %s", method))
	argType := m.Type.In(1)
	must.Eq(t, reflect.Ptr, argType.Kind())
	return reflect.New(argType.Elem()).Interface()
}

// TestAuditOperations_endpoints checks that every recorded operation is
// recorded by its endpoint and every endpoint that records one is in the
// table, and that the parameters of each come from the request type its
// endpoint takes.
func TestAuditOperations_endpoints(t *testing.T) {
	ci.Parallel(t)

	called := map[string]bool{}
	files, err := os.ReadDir(".")
	must.NoError(t, err)
	call := regexp.MustCompile(`auditCall\("([A-Za-z]+\.[A-Za-z]+)"`)
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		must.NoError(t, err)
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			called[m[1]] = true
		}
	}
	for method := range called {
		_, ok := auditOperations[method]
		must.True(t, ok, must.Sprintf("%s is recorded but not in auditOperations", method))
	}
	for method, op := range auditOperations {
		must.True(t, called[method], must.Sprintf("%s is in auditOperations but its endpoint records nothing", method))
		if op.params == nil {
			continue
		}
		args := auditRequest(t, method)
		fillAuditMarkers(reflect.ValueOf(args).Elem(), "")
		must.SliceNotEmpty(t, op.params(args), must.Sprintf("%s: the parameters are not read from %T", method, args))
	}
}

// auditSecretFields are fields of the recorded requests that hold secrets.
var auditSecretFields = []string{"AuthToken", "Key", "CoreKey", "AgeIdentity", "Cert", "Confirm"}

// fillAuditMarkers sets every string and byte slice reachable from v to a
// marker naming its field (SECRET<field path>).
func fillAuditMarkers(v reflect.Value, path string) {
	if strings.Count(path, ".") > 8 {
		return // a recursive type
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("SECRET" + path)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte("SECRET" + path))
			return
		}
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillAuditMarkers(s.Index(0), path)
		v.Set(s)
	case reflect.Ptr:
		if v.Type().Elem().Kind() == reflect.Struct && v.Type().Elem().PkgPath() == "net" {
			return
		}
		n := reflect.New(v.Type().Elem())
		fillAuditMarkers(n.Elem(), path)
		v.Set(n)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		e := reflect.New(v.Type().Elem()).Elem()
		fillAuditMarkers(k, path+".key")
		fillAuditMarkers(e, path)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() || f.Type.PkgPath() == "time" {
				continue
			}
			fillAuditMarkers(v.Field(i), path+"."+f.Name)
		}
	}
}

// TestAuditRecord_noSecrets checks that no secret field of any recorded
// request enters its record, with every field of the request set.
func TestAuditRecord_noSecrets(t *testing.T) {
	ci.Parallel(t)

	sink := &auditSink{}
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Level: hclog.Off})
	logger.RegisterSink(sink)
	srv := &Server{config: &Config{NodeName: "server-1", Region: "global"}, logger: logger}

	for method := range auditOperations {
		t.Run(method, func(t *testing.T) {
			args := auditRequest(t, method)
			fillAuditMarkers(reflect.ValueOf(args).Elem(), "")
			req, ok := args.(structs.RequestWithIdentity)
			must.True(t, ok)
			req.SetIdentity(&structs.AuthenticatedIdentity{ACLToken: &structs.ACLToken{
				AccessorID: "accessor-1", SecretID: "SECRET.SecretID", Name: "operator",
			}})
			err := errors.New("failed")
			srv.auditCall(method, req).done(&err)

			records := sink.take()
			must.Len(t, 1, records)
			line := fmt.Sprint(records[0])
			for _, field := range auditSecretFields {
				must.StrNotContains(t, line, "."+field+" ")
				must.StrNotContains(t, line, "."+field+"]")
			}
			must.StrNotContains(t, line, "SECRET.SecretID")
			must.Eq[any](t, true, records[0]["audit"])
			must.Eq[any](t, method, records[0]["operation"])
			must.Eq[any](t, "accessor-1", records[0]["accessor_id"])
		})
	}
}

// TestAuditRecord_fields checks the fields of a record: the identity of the
// caller and the result.
func TestAuditRecord_fields(t *testing.T) {
	ci.Parallel(t)

	sink := &auditSink{}
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Level: hclog.Off})
	logger.RegisterSink(sink)
	srv := &Server{config: &Config{NodeName: "server-1", Region: "west"}, logger: logger}

	tests := []struct {
		name     string
		identity *structs.AuthenticatedIdentity
		err      error
		expected map[string]any
	}{
		{
			name:     "token success",
			identity: &structs.AuthenticatedIdentity{ACLToken: &structs.ACLToken{AccessorID: "a1", Name: "ops"}},
			expected: map[string]any{"identity": "token:a1", "accessor_id": "a1", "token_name": "ops", "result": "success"},
		},
		{
			name:     "denied",
			identity: &structs.AuthenticatedIdentity{ACLToken: structs.AnonymousACLToken},
			err:      structs.ErrPermissionDenied,
			expected: map[string]any{"identity": "token:anonymous", "result": "denied", "error": structs.ErrPermissionDenied},
		},
		{
			name: "workload identity error",
			identity: &structs.AuthenticatedIdentity{Claims: &structs.IdentityClaims{
				AllocationID: "alloc-1", JobID: "web", Namespace: "prod", TaskName: "app",
			}},
			err: errors.New("boom"),
			expected: map[string]any{"identity": "alloc:alloc-1", "alloc_id": "alloc-1", "job_id": "web",
				"namespace": "prod", "task": "app", "result": "error"},
		},
		{
			name:     "client",
			identity: &structs.AuthenticatedIdentity{ClientID: "node-1"},
			expected: map[string]any{"identity": "client:node-1"},
		},
		{
			name:     "unauthenticated",
			expected: map[string]any{"identity": "unauthenticated"},
		},
		{
			name:     "in-process without ACLs",
			identity: &structs.AuthenticatedIdentity{},
			expected: map[string]any{"identity": "anonymous"},
		},
		{
			name:     "remote address only",
			identity: &structs.AuthenticatedIdentity{RemoteIP: net.ParseIP("10.0.0.7")},
			expected: map[string]any{"identity": "ip:10.0.0.7", "remote_ip": "10.0.0.7"},
		},
		{
			name:     "TLS name and address",
			identity: &structs.AuthenticatedIdentity{TLSName: "server.global.nomad", RemoteIP: net.ParseIP("10.0.0.8")},
			expected: map[string]any{"identity": "server.global.nomad:10.0.0.8"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := &structs.NodeUpdateEligibilityRequest{NodeID: "node-9", Eligibility: structs.NodeSchedulingIneligible}
			args.SetIdentity(tc.identity)
			err := tc.err
			srv.auditCall("Node.UpdateEligibility", args).done(&err)

			records := sink.take()
			must.Len(t, 1, records)
			rec := records[0]
			must.Eq[any](t, hclog.Info, rec["@level"])
			must.Eq[any](t, "audit", rec["@module"])
			must.Eq[any](t, "Node.UpdateEligibility", rec["operation"])
			must.Eq[any](t, "server-1", rec["server"])
			must.Eq[any](t, "west", rec["region"])
			for k, v := range tc.expected {
				must.Eq[any](t, fmt.Sprint(v), fmt.Sprint(rec[k]), must.Sprintf("key %s", k))
			}
			must.Eq[any](t, "node-9", rec["node_id"])
			must.Eq[any](t, structs.NodeSchedulingIneligible, rec["eligibility"])
		})
	}

	t.Run("forwarded", func(t *testing.T) {
		args := &structs.SealUnsealRequest{ServerID: "server-2"}
		rec := srv.auditCall("Operator.SealUnseal", args)
		rec.forwarded()
		var err error
		rec.done(&err)
		must.SliceEmpty(t, sink.take())
	})
}

// TestAudit_operatorWrites checks the records of operator write calls on an
// ACL-enabled server: one per call, none for a read, the denied ones too,
// never the unseal share or the recovered key.
func TestAudit_operatorWrites(t *testing.T) {
	ci.Parallel(t)

	sink := &auditSink{}
	s1, root, cleanupS1 := TestACLServer(t, withAuditSink(sink))
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	sink.take()

	write := func(region string, token string) structs.WriteRequest {
		return structs.WriteRequest{Region: region, AuthToken: token}
	}
	region := s1.config.Region

	t.Run("scheduler configuration", func(t *testing.T) {
		args := &structs.SchedulerSetConfigRequest{
			Config:       structs.SchedulerConfiguration{SchedulerAlgorithm: structs.SchedulerAlgorithmSpread},
			WriteRequest: write(region, root.SecretID),
		}
		var reply structs.SchedulerSetConfigurationResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.SchedulerSetConfiguration", args, &reply))

		records := sink.take()
		must.Len(t, 1, records)
		rec := records[0]
		must.Eq[any](t, "Operator.SchedulerSetConfiguration", rec["operation"])
		must.Eq[any](t, "success", rec["result"])
		must.Eq[any](t, root.AccessorID, rec["accessor_id"])
		must.Eq[any](t, root.Name, rec["token_name"])
		must.Eq[any](t, string(structs.SchedulerAlgorithmSpread), rec["scheduler_algorithm"])
		must.Eq[any](t, s1.config.NodeName, rec["server"])
		must.StrNotContains(t, sink.String(), root.SecretID)
	})

	t.Run("denied", func(t *testing.T) {
		args := &structs.SchedulerSetConfigRequest{WriteRequest: write(region, "")}
		var reply structs.SchedulerSetConfigurationResponse
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerSetConfiguration", args, &reply)
		must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, "denied", records[0]["result"])
		must.Eq[any](t, "token:anonymous", records[0]["identity"])
	})

	t.Run("read is not recorded", func(t *testing.T) {
		args := &structs.GenericRequest{QueryOptions: structs.QueryOptions{Region: region, AuthToken: root.SecretID}}
		var reply structs.SchedulerConfigurationResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.SchedulerGetConfiguration", args, &reply))
		must.SliceEmpty(t, sink.take())
	})

	t.Run("raft peer error", func(t *testing.T) {
		args := &structs.RaftPeerByIDRequest{ID: "not-a-peer", WriteRequest: write(region, root.SecretID)}
		var reply struct{}
		must.Error(t, msgpackrpc.CallWithCodec(codec, "Operator.RaftRemovePeerByID", args, &reply))

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, "error", records[0]["result"])
		must.Eq[any](t, "not-a-peer", records[0]["server_id"])
	})

	t.Run("system gc", func(t *testing.T) {
		args := &structs.GenericRequest{QueryOptions: structs.QueryOptions{Region: region, AuthToken: root.SecretID}}
		var reply structs.GenericResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "System.GarbageCollect", args, &reply))

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, "System.GarbageCollect", records[0]["operation"])
		must.Eq[any](t, "success", records[0]["result"])
	})

	t.Run("node eligibility", func(t *testing.T) {
		node := mock.Node()
		must.NoError(t, s1.State().UpsertNode(structs.MsgTypeTestSetup, 1000, node))
		args := &structs.NodeUpdateEligibilityRequest{
			NodeID:       node.ID,
			Eligibility:  structs.NodeSchedulingIneligible,
			WriteRequest: write(region, root.SecretID),
		}
		var reply structs.NodeEligibilityUpdateResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.UpdateEligibility", args, &reply))

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, node.ID, records[0]["node_id"])
		must.Eq[any](t, structs.NodeSchedulingIneligible, records[0]["eligibility"])
		must.Eq[any](t, "success", records[0]["result"])
	})

	t.Run("keyring rotate", func(t *testing.T) {
		args := &structs.KeyringRotateRootKeyRequest{WriteRequest: write(region, root.SecretID)}
		var reply structs.KeyringRotateRootKeyResponse
		_ = msgpackrpc.CallWithCodec(codec, "Keyring.Rotate", args, &reply)

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, "Keyring.Rotate", records[0]["operation"])
		must.MapContainsKey(t, records[0], "algorithm")
	})

	t.Run("unseal share never logged", func(t *testing.T) {
		const share = "c2hhcmUtdGhhdC1tdXN0LW5ldmVyLWJlLWxvZ2dlZA=="
		args := &structs.SealUnsealRequest{Key: share, WriteRequest: write(region, root.SecretID)}
		var reply structs.SealUnsealResponse
		_ = msgpackrpc.CallWithCodec(codec, "Operator.SealUnseal", args, &reply)

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, "Operator.SealUnseal", records[0]["operation"])
		must.StrNotContains(t, sink.String(), share)
	})

	t.Run("recovery material never logged", func(t *testing.T) {
		const share = "cmVjb3Zlcnktc2hhcmUtbmV2ZXItbG9nZ2Vk"
		coreKey := []byte("core-key-never-logged")
		args := &structs.SealRecoverRequest{Key: share, CoreKey: coreKey, WriteRequest: write(region, root.SecretID)}
		var reply structs.SealRecoverResponse
		_ = msgpackrpc.CallWithCodec(codec, "Operator.SealRecover", args, &reply)

		records := sink.take()
		must.Len(t, 1, records)
		must.Eq[any](t, true, records[0]["with_share"])
		must.Eq[any](t, true, records[0]["with_core_key"])
		must.StrNotContains(t, sink.String(), share)
		must.StrNotContains(t, sink.String(), string(coreKey))
		must.StrNotContains(t, sink.String(), fmt.Sprint(coreKey))
	})
}

// TestAudit_forwarded checks that a call forwarded to the leader is recorded
// once, by the leader.
func TestAudit_forwarded(t *testing.T) {
	ci.Parallel(t)

	sink1, sink2 := &auditSink{}, &auditSink{}
	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		withAuditSink(sink1)(c)
	})
	defer cleanupS1()
	s2, cleanupS2 := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		withAuditSink(sink2)(c)
	})
	defer cleanupS2()
	TestJoin(t, s1, s2)
	testutil.WaitForLeaders(t, s1.RPC, s2.RPC)

	follower, leaderSink, followerSink := s1, sink2, sink1
	if s1.IsLeader() {
		follower, leaderSink, followerSink = s2, sink1, sink2
	}
	leaderSink.take()
	followerSink.take()

	args := &structs.SchedulerSetConfigRequest{
		Config:       structs.SchedulerConfiguration{SchedulerAlgorithm: structs.SchedulerAlgorithmBinpack},
		WriteRequest: structs.WriteRequest{Region: follower.config.Region},
	}
	var reply structs.SchedulerSetConfigurationResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, follower), "Operator.SchedulerSetConfiguration", args, &reply))

	must.SliceEmpty(t, followerSink.take())
	records := leaderSink.take()
	must.Len(t, 1, records)
	must.Eq[any](t, "success", records[0]["result"])
}
