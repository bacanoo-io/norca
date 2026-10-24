// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/base/{plugin,client,server}.go,
// plugins/apm/{plugin,client,server}.go and
// plugins/strategy/{plugin,client,server}.go.
// Modified: merged into one package working with the in-process plugin
// interfaces of the built-in autoscaler; every client call has a timeout so
// a hung plugin process cannot block an evaluation forever. The gRPC services
// and messages are unchanged, so plugins built with the autoscaler plugin SDK
// talk to the server without changes.

package external

import (
	"context"
	"fmt"
	"time"

	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	apmproto "github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/proto/v1"
	baseproto "github.com/hashicorp/nomad/nomad/autoscaler/plugins/base/proto/v1"
	sharedproto "github.com/hashicorp/nomad/nomad/autoscaler/plugins/shared/proto/v1"
	strategyproto "github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/proto/v1"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"google.golang.org/grpc"
)

// CallTimeout bounds every gRPC call made to an external plugin.
var CallTimeout = time.Minute

// ---------------------------------------------------------------------------
// Base

// PluginBase is the Base implementation of the go-plugin GRPCPlugin interface.
type PluginBase struct {
	// Embedded so we disable support for net/rpc based plugins.
	plugin.NetRPCUnsupportedPlugin

	// Impl is the Base interface implementation that the plugin serves.
	Impl plugins.Base
}

// GRPCServer is the Base implementation of the go-plugin GRPCPlugin.GRPCServer
// interface function.
func (p *PluginBase) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	baseproto.RegisterBasePluginServiceServer(s, &baseServer{impl: p.Impl})
	return nil
}

// GRPCClient is the Base implementation of the go-plugin GRPCPlugin.GRPCClient
// interface function.
func (p *PluginBase) GRPCClient(ctx context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &baseClient{client: baseproto.NewBasePluginServiceClient(c), doneCtx: ctx}, nil
}

// baseClient is the gRPC client implementation of the Base interface.
type baseClient struct {
	client  baseproto.BasePluginServiceClient
	doneCtx context.Context
}

// callCtx returns the context of one call to the plugin.
func callCtx(doneCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(doneCtx, CallTimeout)
}

// PluginInfo is the gRPC client implementation of the Base.PluginInfo
// interface function.
func (p *baseClient) PluginInfo() (*plugins.PluginInfo, error) {
	ctx, cancel := callCtx(p.doneCtx)
	defer cancel()

	info, err := p.client.PluginInfo(ctx, &baseproto.PluginInfoRequest{})
	if err != nil {
		return nil, err
	}

	var pType string
	switch info.GetType() {
	case baseproto.PluginType_PLUGIN_TYPE_APM:
		pType = sdk.PluginTypeAPM
	case baseproto.PluginType_PLUGIN_TYPE_STRATEGY:
		pType = sdk.PluginTypeStrategy
	case baseproto.PluginType_PLUGIN_TYPE_TARGET:
		pType = sdk.PluginTypeTarget
	default:
		return nil, fmt.Errorf("plugin is of unknown type: %q", info.GetType().String())
	}

	return &plugins.PluginInfo{PluginType: pType, Name: info.GetName()}, nil
}

// SetConfig is the gRPC client implementation of the Base.SetConfig interface
// function.
func (p *baseClient) SetConfig(cfg map[string]string) error {
	ctx, cancel := callCtx(p.doneCtx)
	defer cancel()

	_, err := p.client.SetConfig(ctx, &baseproto.SetConfigRequest{Config: cfg})
	return err
}

// baseServer is the gRPC server implementation of the Base interface.
type baseServer struct {
	baseproto.UnimplementedBasePluginServiceServer
	impl plugins.Base
}

// PluginInfo is the gRPC server implementation of the Base.PluginInfo
// interface function.
func (p *baseServer) PluginInfo(_ context.Context, _ *baseproto.PluginInfoRequest) (*baseproto.PluginInfoResponse, error) {
	info, err := p.impl.PluginInfo()
	if err != nil {
		return nil, err
	}

	var pType baseproto.PluginType
	switch info.PluginType {
	case sdk.PluginTypeAPM:
		pType = baseproto.PluginType_PLUGIN_TYPE_APM
	case sdk.PluginTypeStrategy:
		pType = baseproto.PluginType_PLUGIN_TYPE_STRATEGY
	case sdk.PluginTypeTarget:
		pType = baseproto.PluginType_PLUGIN_TYPE_TARGET
	default:
		return nil, fmt.Errorf("plugin is of unknown type: %q", info.PluginType)
	}

	return &baseproto.PluginInfoResponse{Type: pType, Name: info.Name}, nil
}

// SetConfig is the gRPC server implementation of the Base.SetConfig interface
// function.
func (p *baseServer) SetConfig(_ context.Context, req *baseproto.SetConfigRequest) (*baseproto.SetConfigResponse, error) {
	return &baseproto.SetConfigResponse{}, p.impl.SetConfig(req.GetConfig())
}

// ---------------------------------------------------------------------------
// APM

// PluginAPM is the APM implementation of the go-plugin GRPCPlugin interface.
type PluginAPM struct {
	// Embedded so we disable support for net/rpc based plugins.
	plugin.NetRPCUnsupportedPlugin

	// Impl is the APM interface implementation that the plugin serves.
	Impl plugins.APM
}

// GRPCServer is the APM implementation of the go-plugin GRPCPlugin.GRPCServer
// interface function.
func (p *PluginAPM) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	apmproto.RegisterAPMPluginServiceServer(s, &apmServer{impl: p.Impl})
	return nil
}

// GRPCClient is the APM implementation of the go-plugin GRPCPlugin.GRPCClient
// interface function.
func (p *PluginAPM) GRPCClient(ctx context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &apmClient{
		baseClient: &baseClient{client: baseproto.NewBasePluginServiceClient(c), doneCtx: ctx},
		client:     apmproto.NewAPMPluginServiceClient(c),
	}, nil
}

// apmClient is the gRPC client implementation of the APM interface.
type apmClient struct {
	*baseClient
	client apmproto.APMPluginServiceClient
}

// Query is the gRPC client implementation of the APM.Query interface
// function.
func (p *apmClient) Query(query string, timeRange sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	ctx, cancel := callCtx(p.doneCtx)
	defer cancel()

	resp, err := p.client.Query(ctx, &apmproto.QueryRequest{Query: query, TimeRange: timeRangeToProto(timeRange)})
	if err != nil {
		return nil, err
	}
	return protoToTimestampedMetrics(resp.GetTimestampedMetric()), nil
}

// QueryMultiple is the gRPC client implementation of the APM.QueryMultiple
// interface function.
func (p *apmClient) QueryMultiple(query string, timeRange sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	ctx, cancel := callCtx(p.doneCtx)
	defer cancel()

	resp, err := p.client.QueryMultiple(ctx, &apmproto.QueryMultipleRequest{Query: query, TimeRange: timeRangeToProto(timeRange)})
	if err != nil {
		return nil, err
	}

	out := make([]sdk.TimestampedMetrics, len(resp.GetTimestampedMetric()))
	for i, m := range resp.GetTimestampedMetric() {
		out[i] = protoToTimestampedMetrics(m.GetTimestampedMetric())
	}
	return out, nil
}

// apmServer is the gRPC server implementation of the APM interface.
type apmServer struct {
	apmproto.UnimplementedAPMPluginServiceServer
	impl plugins.APM
}

// Query is the gRPC server implementation of the APM.Query interface
// function.
func (p *apmServer) Query(_ context.Context, req *apmproto.QueryRequest) (*apmproto.QueryResponse, error) {
	res, err := p.impl.Query(req.GetQuery(), protoToTimeRange(req.GetTimeRange()))
	if err != nil {
		return nil, err
	}
	return &apmproto.QueryResponse{TimestampedMetric: timestampedMetricsToProto(res)}, nil
}

// QueryMultiple is the gRPC server implementation of the APM.QueryMultiple
// interface function.
func (p *apmServer) QueryMultiple(_ context.Context, req *apmproto.QueryMultipleRequest) (*apmproto.QueryMultipleResponse, error) {
	res, err := p.impl.QueryMultiple(req.GetQuery(), protoToTimeRange(req.GetTimeRange()))
	if err != nil {
		return nil, err
	}

	out := make([]*apmproto.QueryResponse, len(res))
	for i, m := range res {
		out[i] = &apmproto.QueryResponse{TimestampedMetric: timestampedMetricsToProto(m)}
	}
	return &apmproto.QueryMultipleResponse{TimestampedMetric: out}, nil
}

// ---------------------------------------------------------------------------
// Strategy

// PluginStrategy is the Strategy implementation of the go-plugin GRPCPlugin
// interface.
type PluginStrategy struct {
	// Embedded so we disable support for net/rpc based plugins.
	plugin.NetRPCUnsupportedPlugin

	// Impl is the Strategy interface implementation that the plugin serves.
	Impl plugins.Strategy
}

// GRPCServer is the Strategy implementation of the go-plugin
// GRPCPlugin.GRPCServer interface function.
func (p *PluginStrategy) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	strategyproto.RegisterStrategyPluginServiceServer(s, &strategyServer{impl: p.Impl})
	return nil
}

// GRPCClient is the Strategy implementation of the go-plugin
// GRPCPlugin.GRPCClient interface function.
func (p *PluginStrategy) GRPCClient(ctx context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &strategyClient{
		baseClient: &baseClient{client: baseproto.NewBasePluginServiceClient(c), doneCtx: ctx},
		client:     strategyproto.NewStrategyPluginServiceClient(c),
	}, nil
}

// strategyClient is the gRPC client implementation of the Strategy
// interface.
type strategyClient struct {
	*baseClient
	client strategyproto.StrategyPluginServiceClient
}

// Run is the gRPC client implementation of the Strategy.Run interface
// function.
func (p *strategyClient) Run(eval *sdk.ScalingCheckEvaluation, count int64) (*sdk.ScalingCheckEvaluation, error) {
	ctx, cancel := callCtx(p.doneCtx)
	defer cancel()

	resp, err := p.client.Run(ctx, &strategyproto.RunRequest{
		Action:            &sharedproto.ScalingAction{},
		Count:             count,
		Check:             scalingPolicyCheckToProto(eval.Check),
		TimestampedMetric: timestampedMetricsToProto(eval.Metrics),
	})
	if err != nil {
		return nil, err
	}

	action, err := protoToScalingAction(resp.GetAction())
	if err != nil {
		return nil, err
	}

	// Keep the meta the host set on the action, such as the policy ID.
	if eval.Action != nil {
		for k, v := range eval.Action.Meta {
			if _, ok := action.Meta[k]; !ok {
				action.Meta[k] = v
			}
		}
	}

	// Update the eval with the new action and return.
	eval.Action = &action
	return eval, nil
}

// strategyServer is the gRPC server implementation of the Strategy
// interface.
type strategyServer struct {
	strategyproto.UnimplementedStrategyPluginServiceServer
	impl plugins.Strategy
}

// Run is the gRPC server implementation of the Strategy.Run interface
// function.
func (p *strategyServer) Run(_ context.Context, req *strategyproto.RunRequest) (*strategyproto.RunResponse, error) {
	// Populate the eval. At this point of the evaluation flow we will only
	// have Check and Metrics sections populated, so only translate this.
	eval := sdk.ScalingCheckEvaluation{
		Action:  &sdk.ScalingAction{},
		Check:   protoToScalingPolicyCheck(req.GetCheck()),
		Metrics: protoToTimestampedMetrics(req.GetTimestampedMetric()),
	}

	resp, err := p.impl.Run(&eval, req.GetCount())
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Action == nil {
		return nil, fmt.Errorf("strategy returned no action")
	}

	// Populate the action and re-use the request Check and metrics so we
	// don't have to incur time penalties on data which hasn't been touched
	// or changed.
	action, err := scalingActionToProto(*resp.Action)
	if err != nil {
		return nil, err
	}

	return &strategyproto.RunResponse{
		Action:            action,
		Check:             req.GetCheck(),
		TimestampedMetric: req.GetTimestampedMetric(),
	}, nil
}
