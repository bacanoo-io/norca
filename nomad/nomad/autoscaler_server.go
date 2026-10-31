// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/structs"
)

// autoscalerACLPolicy is the only permission of the built-in autoscaler's
// system identity: it may scale jobs in any namespace, and write the scale
// subresource and the event log of custom objects, nothing else.
const autoscalerACLPolicy = `namespace "*" {
  capabilities = ["scale-job"]
  object "*" { capabilities = ["write", "write-events"] }
}`

var (
	// autoscalerACLToken is the system identity of the built-in autoscaler.
	// It is never stored in the state store; the leader recognizes it by the
	// in-memory secret generated when leadership is established.
	autoscalerACLToken = &structs.ACLToken{
		AccessorID: "autoscaler",
		Name:       "Built-in autoscaler",
		Type:       structs.ACLClientToken,
		Global:     false,
	}

	// autoscalerACL is the resolved ACL of autoscalerACLToken.
	autoscalerACL = func() *acl.ACL {
		policy, err := acl.Parse(autoscalerACLPolicy)
		if err != nil {
			panic(err)
		}
		aclObj, err := acl.NewACL(false, []*acl.Policy{policy})
		if err != nil {
			panic(err)
		}
		return aclObj
	}()
)

// setupAutoscaler creates the built-in autoscaler. It runs only on the leader
// (see establishLeadership) and only when enabled in the configuration.
func (s *Server) setupAutoscaler() {
	cfg := s.config.Autoscaler
	if cfg == nil {
		cfg = autoscaler.DefaultConfig()
	}

	s.autoscaler = autoscaler.New(s.logger, cfg, &autoscaler.Deps{
		State:        s.State,
		Scaler:       &autoscalerJobScaler{srv: s},
		ObjectScaler: &autoscalerJobScaler{srv: s},
		PublishEvent: s.publishScalingEvent,
		UsageHistory: s.autoscalerUsageHistory,
		ServersReady: func() bool {
			return ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
		},
	})
}

// setAutoscalerSecret sets the secret of the autoscaler's system identity.
// It is only valid on the current leader.
func (s *Server) setAutoscalerSecret(secret string) {
	s.autoscalerSecretLock.Lock()
	defer s.autoscalerSecretLock.Unlock()
	s.autoscalerSecret = secret
}

// getAutoscalerSecret returns the secret of the autoscaler's system identity.
func (s *Server) getAutoscalerSecret() string {
	s.autoscalerSecretLock.Lock()
	defer s.autoscalerSecretLock.Unlock()
	return s.autoscalerSecret
}

// isAutoscalerSecret reports whether secretID is the secret of the
// autoscaler's system identity.
func (s *Server) isAutoscalerSecret(secretID string) bool {
	secret := s.getAutoscalerSecret()
	return secret != "" && secretID == secret
}

// autoscalerDefaultEngine returns the engine of scaling policies that do not
// set one.
func (s *Server) autoscalerDefaultEngine() string {
	if s.autoscaler == nil {
		return structs.ScalingPolicyEngineExternal
	}
	return s.autoscaler.DefaultEngine()
}

// autoscalerJobScaler executes the autoscaler's decisions through the
// existing Job.Scale endpoint, as an internal call. This keeps the ACL check,
// the scaling_event and the evaluation creation of Job.Scale unchanged.
type autoscalerJobScaler struct {
	srv *Server
}

func (a *autoscalerJobScaler) Scale(args *structs.JobScaleRequest) (*structs.JobRegisterResponse, error) {
	secret := a.srv.getAutoscalerSecret()
	if secret == "" {
		return nil, fmt.Errorf("not the leader")
	}
	args.Region = a.srv.Region()
	args.AuthToken = secret

	var reply structs.JobRegisterResponse
	if err := a.srv.RPC("Job.Scale", args, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// ScaleObject executes a decision on a custom object through its scale
// subresource, as an internal call with the autoscaler's identity.
func (a *autoscalerJobScaler) ScaleObject(args *structs.CustomObjectScaleRequest) (*structs.CustomObjectScaleResponse, error) {
	secret := a.srv.getAutoscalerSecret()
	if secret == "" {
		return nil, fmt.Errorf("not the leader")
	}
	args.Region = a.srv.Region()
	args.AuthToken = secret

	var reply structs.CustomObjectScaleResponse
	if err := a.srv.RPC("CustomObject.Scale", args, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// RecordObjectEvents writes events of the autoscaler to the event log of a
// custom object.
func (a *autoscalerJobScaler) RecordObjectEvents(args *structs.CustomObjectEventsRequest) error {
	secret := a.srv.getAutoscalerSecret()
	if secret == "" {
		return fmt.Errorf("not the leader")
	}
	args.Region = a.srv.Region()
	args.AuthToken = secret

	var reply structs.GenericResponse
	return a.srv.RPC("CustomObject.RecordEvents", args, &reply)
}

// validateScalingEngineFilter validates the Engine filter of a scaling policy
// list request.
func validateScalingEngineFilter(engine string) error {
	switch engine {
	case "", structs.ScalingPolicyEngineBuiltin, structs.ScalingPolicyEngineExternal:
		return nil
	default:
		return structs.NewErrRPCCoded(400, fmt.Sprintf("engine must be %q or %q",
			structs.ScalingPolicyEngineBuiltin, structs.ScalingPolicyEngineExternal))
	}
}

// scalingPolicyMatchesEngine reports whether the policy passes the Engine
// filter of a list request.
func scalingPolicyMatchesEngine(p *structs.ScalingPolicy, engine, defaultEngine string) bool {
	return engine == "" || autoscaler.PolicyEngine(p, defaultEngine) == engine
}

// publishScalingEvent publishes an event of the Scaling topic. The event is
// not written to Raft, so it is only published on the leader that runs the
// built-in autoscaler.
func (s *Server) publishScalingEvent(eventType string, ev *structs.ScalingDecisionEvent) {
	store := s.State()
	if store == nil {
		return
	}
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:      structs.TopicScaling,
			Type:       eventType,
			Key:        ev.JobID,
			Namespace:  ev.Namespace,
			FilterKeys: []string{ev.Group, ev.PolicyID},
			Index:      index,
			Payload:    ev,
		}},
	})
}
