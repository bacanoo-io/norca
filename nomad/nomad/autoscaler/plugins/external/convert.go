// Copyright IBM Corp. 2020, 2025
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/shared/util.go.
// Modified: unexported, nil-safe on malformed messages from a plugin, and
// adapted to the in-process sdk package of the built-in autoscaler.

package external

import (
	"encoding/json"
	"fmt"

	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/shared/proto/v1"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// scalingDirectionToProto converts the input scale direction to the proto
// equivalent.
func scalingDirectionToProto(input sdk.ScaleDirection) (proto.ScalingDirection, error) {
	switch input {
	case sdk.ScaleDirectionDown:
		return proto.ScalingDirection_SCALING_DIRECTION_DOWN, nil
	case sdk.ScaleDirectionUp:
		return proto.ScalingDirection_SCALING_DIRECTION_UP, nil
	case sdk.ScaleDirectionNone:
		return proto.ScalingDirection_SCALING_DIRECTION_NONE, nil
	default:
		return proto.ScalingDirection_SCALING_DIRECTION_UNSPECIFIED,
			fmt.Errorf("scale direction is unknown: %q", input)
	}
}

// protoToScalingDirection converts the input proto definition of
// ScalingDirection and returns the Autoscaler equivalent.
func protoToScalingDirection(input proto.ScalingDirection) (sdk.ScaleDirection, error) {
	switch input {
	case proto.ScalingDirection_SCALING_DIRECTION_DOWN:
		return sdk.ScaleDirectionDown, nil
	case proto.ScalingDirection_SCALING_DIRECTION_UP:
		return sdk.ScaleDirectionUp, nil
	case proto.ScalingDirection_SCALING_DIRECTION_NONE:
		return sdk.ScaleDirectionNone, nil
	default:
		return sdk.ScaleDirectionNone, fmt.Errorf("scale direction is unknown: %q", input)
	}
}

// timeRangeToProto takes an input time range and converts it to the proto
// equivalent.
func timeRangeToProto(input sdk.TimeRange) *proto.TimeRange {
	return &proto.TimeRange{
		To:   timestamppb.New(input.To),
		From: timestamppb.New(input.From),
	}
}

// protoToTimeRange converts the input proto definition of TimeRange and
// returns the Autoscaler equivalent.
func protoToTimeRange(input *proto.TimeRange) sdk.TimeRange {
	return sdk.TimeRange{
		To:   input.GetTo().AsTime(),
		From: input.GetFrom().AsTime(),
	}
}

// timestampedMetricsToProto converts the input TimestampedMetrics to the
// proto equivalent.
func timestampedMetricsToProto(input sdk.TimestampedMetrics) []*proto.TimestampedMetric {
	out := make([]*proto.TimestampedMetric, len(input))
	for i, m := range input {
		out[i] = &proto.TimestampedMetric{Timestamp: timestamppb.New(m.Timestamp), Value: m.Value}
	}
	return out
}

// protoToTimestampedMetrics converts the input proto TimestampedMetric
// objects and returns the Autoscaler equivalent. Nil entries are skipped.
func protoToTimestampedMetrics(input []*proto.TimestampedMetric) sdk.TimestampedMetrics {
	out := make(sdk.TimestampedMetrics, 0, len(input))
	for _, m := range input {
		if m == nil {
			continue
		}
		out = append(out, sdk.TimestampedMetric{Timestamp: m.GetTimestamp().AsTime(), Value: m.GetValue()})
	}
	return out
}

// actionMetaToProto converts the input meta map to the proto equivalent.
func actionMetaToProto(input map[string]interface{}) (*anypb.Any, error) {
	if input == nil {
		return nil, nil
	}
	out, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return &anypb.Any{Value: out}, nil
}

// protoToActionMeta converts the input proto any object and returns the meta
// map equivalent.
func protoToActionMeta(input *anypb.Any) (map[string]interface{}, error) {
	if input == nil || len(input.GetValue()) == 0 {
		return nil, nil
	}
	out := make(map[string]interface{})
	err := json.Unmarshal(input.GetValue(), &out)
	return out, err
}

// scalingActionToProto converts the input ScalingAction to the proto
// equivalent.
func scalingActionToProto(input sdk.ScalingAction) (*proto.ScalingAction, error) {
	dir, err := scalingDirectionToProto(input.Direction)
	if err != nil {
		return nil, err
	}

	meta, err := actionMetaToProto(input.Meta)
	if err != nil {
		return nil, err
	}

	return &proto.ScalingAction{
		Count:     input.Count,
		Reason:    input.Reason,
		Error:     input.Error,
		Direction: dir,
		Meta:      meta,
	}, nil
}

// protoToScalingAction converts the input proto ScalingAction object and
// returns the Autoscaler equivalent.
func protoToScalingAction(input *proto.ScalingAction) (sdk.ScalingAction, error) {
	var out sdk.ScalingAction

	if input == nil {
		out.Canonicalize()
		return out, nil
	}

	dir, err := protoToScalingDirection(input.GetDirection())
	if err != nil {
		return out, err
	}
	meta, err := protoToActionMeta(input.GetMeta())
	if err != nil {
		return out, err
	}

	out.Count = input.GetCount()
	out.Reason = input.GetReason()
	out.Error = input.GetError()
	out.Direction = dir
	out.Meta = meta
	out.Canonicalize()

	return out, nil
}

// scalingPolicyCheckToProto converts the input ScalingPolicyCheck to the
// proto equivalent.
func scalingPolicyCheckToProto(input *sdk.ScalingPolicyCheck) *proto.ScalingPolicyCheck {
	if input == nil {
		return nil
	}
	out := &proto.ScalingPolicyCheck{
		Name:        input.Name,
		Source:      input.Source,
		Query:       input.Query,
		QueryWindow: durationpb.New(input.QueryWindow),
	}
	if input.Strategy != nil {
		out.Strategy = &proto.ScalingPolicyStrategy{
			Name:   input.Strategy.Name,
			Config: input.Strategy.Config,
		}
	}
	return out
}

// protoToScalingPolicyCheck converts the input proto ScalingPolicyCheck
// object and returns the Autoscaler equivalent.
func protoToScalingPolicyCheck(input *proto.ScalingPolicyCheck) *sdk.ScalingPolicyCheck {
	return &sdk.ScalingPolicyCheck{
		Name:        input.GetName(),
		Source:      input.GetSource(),
		Query:       input.GetQuery(),
		QueryWindow: input.GetQueryWindow().AsDuration(),
		Strategy: &sdk.ScalingPolicyStrategy{
			Name:   input.GetStrategy().GetName(),
			Config: input.GetStrategy().GetConfig(),
		},
	}
}
