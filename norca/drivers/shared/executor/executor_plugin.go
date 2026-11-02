// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package executor

import (
	"context"

	"github.com/bacanoo-io/norca/norca/drivers/shared/executor/proto"
	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

type ExecutorPlugin struct {
	// TODO: support backwards compatibility with pre 0.9 NetRPC plugin
	plugin.NetRPCUnsupportedPlugin
	logger        hclog.Logger
	fsIsolation   bool
	cpuTotalTicks uint64
}

func (p *ExecutorPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	if p.fsIsolation {
		proto.RegisterExecutorServer(s, &grpcExecutorServer{impl: NewExecutorWithIsolation(p.logger, p.cpuTotalTicks)})
	} else {
		proto.RegisterExecutorServer(s, &grpcExecutorServer{impl: NewExecutor(p.logger, p.cpuTotalTicks)})
	}
	return nil
}

func (p *ExecutorPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &grpcExecutorClient{
		client:  proto.NewExecutorClient(c),
		doneCtx: ctx,
		logger:  p.logger,
	}, nil
}
