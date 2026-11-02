// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
)

func TestRegionList(t *testing.T) {
	ci.Parallel(t)

	// Make the servers
	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.Region = "region1"
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)

	s2, cleanupS2 := TestServer(t, func(c *Config) {
		c.Region = "region2"
	})
	defer cleanupS2()

	// Join the servers
	s2Addr := fmt.Sprintf("127.0.0.1:%d",
		s2.config.SerfConfig.MemberlistConfig.BindPort)
	if n, err := s1.Join([]string{s2Addr}); err != nil || n != 1 {
		t.Fatalf("Failed joining: %v (%d joined)", err, n)
	}

	// Query the regions list
	testutil.WaitForResult(func() (bool, error) {
		var arg structs.GenericRequest
		var out []string
		if err := msgpackrpc.CallWithCodec(codec, "Region.List", &arg, &out); err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(out) != 2 || out[0] != "region1" || out[1] != "region2" {
			t.Fatalf("unexpected regions: %v", out)
		}
		return true, nil
	}, func(err error) {
		t.Fatalf("err: %v", err)
	})
}
