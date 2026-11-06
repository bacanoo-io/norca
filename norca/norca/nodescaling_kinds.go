// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Node scaling: the capacity groups of nodes and the providers that create
// them are custom objects of two kinds of the group norca.io, registered by
// the core because the built-in autoscaler of the leader relies on them from
// the first moment. People and tools write the objects through the generic
// object API (norca object apply); the kinds themselves can't be changed or
// deleted through it.

const (
	// nodeKindsRetry is how long the leader waits before it tries again to
	// register the kinds (not all servers on a version that supports them,
	// a raft error).
	nodeKindsRetry = 10 * time.Second

	nodeDuration = `"pattern": "^([0-9]+(s|m|h|d))+$"`
	nodeResource = `{"type": "object", "properties": {
        "cpu": {"type": "integer", "minimum": 0},
        "memory": {"type": "integer", "minimum": 0},
        "disk": {"type": "integer", "minimum": 0}}, "additionalProperties": false}`
	nodeStringMap = `{"type": "object", "additionalProperties": {"type": "string"}}`
)

// nodeGroupKindSchema is the JSON Schema of node groups. The settings are in
// the spec; the status is written by the operator that creates the nodes
// (replicas, joining, failed, nodes, capacity, allocatable, rollout,
// lastRequest, join, strayNodes, removal, scaleDown, deletion, conditions) and
// by the autoscaler (decision).
const nodeGroupKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["pool", "datacenter", "max"],
      "properties": {
        "enabled": {"type": "boolean", "default": true},
        "enforce": {"type": "boolean", "default": false},
        "engine": {"type": "string", "enum": ["builtin", "external"], "default": "builtin"},
        "pool": {"type": "string", "minLength": 1},
        "datacenter": {"type": "string", "minLength": 1},
        "node_class": {"type": "string"},
        "replicas": {"type": "integer", "minimum": 0},
        "min": {"type": "integer", "minimum": 0, "default": 0},
        "max": {"type": "integer", "minimum": 0},
        "priority": {"type": "integer", "default": 0},
        "weight": {"type": "integer", "minimum": 1, "default": 1},
        "headroom_nodes": {"type": "integer", "minimum": 0, "default": 0},
        "headroom_percent": {"type": "number", "minimum": 0, "maximum": 100},
        "spill": {"type": "boolean", "default": false},
        "rolling": {
          "type": "object",
          "properties": {
            "mode": {"type": "string", "enum": ["auto", "paused"], "default": "auto"},
            "generation": {"type": "integer", "minimum": 0},
            "max_node_age": {"type": "string", ` + nodeDuration + `},
            "max_unavailable": {"type": "integer", "minimum": 1, "default": 1},
            "max_surge": {"type": "integer", "minimum": 0, "default": 1},
            "jitter": {"type": "string", "pattern": "^[0-9]{1,2}%$"}
          },
          "additionalProperties": false
        },
        "edge": {
          "type": "object",
          "properties": {
            "min_shutdown_delay": {"type": "string", ` + nodeDuration + `}
          },
          "additionalProperties": false
        },
        "deletion": {
          "type": "object",
          "properties": {
            "policy": {"type": "string", "enum": ["drain", "orphan", "terminate"], "default": "drain"},
            "drain_timeout": {"type": "string", ` + nodeDuration + `}
          },
          "additionalProperties": false
        },
        "join": {
          "type": "object",
          "properties": {
            "deadline": {"type": "string", ` + nodeDuration + `},
            "slow_after": {"type": "string", ` + nodeDuration + `},
            "max_failures": {"type": "integer", "minimum": 1},
            "resume_generation": {"type": "integer", "minimum": 0}
          },
          "additionalProperties": false
        },
        "provider": {"type": "string"},
        "group": {"type": "string"},
        "network": {"type": "string", "pattern": "^[0-9]{1,3}(\\.[0-9]{1,3}){3}/([0-9]|[12][0-9]|3[0-2])$"},
        "node": {
          "type": "object",
          "properties": {
            "meta": ` + nodeStringMap + `,
            "attributes": ` + nodeStringMap + `,
            "reserved": ` + nodeResource + `,
            "capacity": ` + nodeResource + `,
            "parameters": {"type": "object"}
          },
          "additionalProperties": false
        },
        "policy": {
          "type": "object",
          "properties": {
            "evaluation_interval": {"type": "string", ` + nodeDuration + `},
            "cooldown": {"type": "string", ` + nodeDuration + `},
            "check": {
              "type": "object",
              "additionalProperties": {
                "type": "object",
                "required": ["source", "query", "strategy"],
                "properties": {
                  "source": {"type": "string", "minLength": 1},
                  "query": {"type": "string", "minLength": 1},
                  "query_window": {"type": "string", ` + nodeDuration + `},
                  "strategy": {"type": "object", "minProperties": 1, "maxProperties": 1,
                               "additionalProperties": {"type": "object"}},
                  "on_error": {"type": "string", "enum": ["fail", "ignore"]}
                }
              }
            },
            "scale_down": {
              "type": "object",
              "properties": {
                "unneeded_for": {"type": "string", ` + nodeDuration + `},
                "delay_after_add": {"type": "string", ` + nodeDuration + `},
                "utilization_below": {"type": "number", "minimum": 0, "maximum": 100},
                "selector": {"type": "string", "enum": ["least_busy", "empty"], "default": "least_busy"},
                "drain_deadline": {"type": "string", ` + nodeDuration + `}
              },
              "additionalProperties": false
            }
          },
          "additionalProperties": false
        }
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "replicas": {"type": "integer", "minimum": 0},
        "joining": {"type": "integer", "minimum": 0},
        "failed": {"type": "integer", "minimum": 0},
        "nodes": {"type": "array", "items": {"type": "object"}},
        "capacity": ` + nodeResource + `,
        "allocatable": ` + nodeResource + `,
        "rollout": {
          "type": "object",
          "properties": {
            "replacing": {
              "type": "array",
              "items": {
                "type": "object",
                "properties": {
                  "instance": {"type": "string"},
                  "name": {"type": "string"},
                  "node": {"type": "string"},
                  "nodeName": {"type": "string"},
                  "reason": {"type": "string", "enum": ["marked", "revision", "generation", "age", "expiry"]},
                  "surge": {"type": "boolean"},
                  "drain": {"type": "boolean"},
                  "phase": {"type": "string", "enum": ["surging", "leaving", "emptying", "draining", "deleted"]},
                  "since": {"type": "string"},
                  "started": {"type": "string"},
                  "expires": {"type": "string"}
                }
              }
            },
            "outdated": {"type": "array", "items": {"type": "string"}},
            "backoff": {
              "type": "object",
              "properties": {
                "vetoes": {"type": "integer", "minimum": 0},
                "retryAfter": {"type": "string"},
                "reason": {"type": "string"},
                "message": {"type": "string"}
              }
            }
          }
        },
        "decision": {"type": "object"},
        "lastRequest": {"type": "integer", "minimum": 0},
        "strayNodes": {"type": "array", "items": {"type": "object"}},
        "removal": {
          "type": "object",
          "properties": {
            "instance": {"type": "string"},
            "name": {"type": "string"},
            "node": {"type": "string"},
            "selector": {"type": "string", "enum": ["least_busy", "empty"]},
            "phase": {"type": "string", "enum": ["leaving", "draining", "settling", "deleting", "deleted"]},
            "started": {"type": "string"},
            "settleUntil": {"type": "string"}
          }
        },
        "scaleDown": {
          "type": "object",
          "properties": {
            "vetoes": {"type": "integer", "minimum": 0},
            "retryAfter": {"type": "string"},
            "reason": {"type": "string"},
            "message": {"type": "string"}
          }
        },
        "deletion": {
          "type": "object",
          "properties": {
            "policy": {"type": "string", "enum": ["drain", "orphan", "terminate"]},
            "deleted": {"type": "object", "additionalProperties": {"type": "string"}},
            "waiting": {"type": "array", "items": {"type": "string"}}
          }
        },
        "join": {
          "type": "object",
          "properties": {
            "failures": {"type": "integer", "minimum": 0},
            "revision": {"type": "string"},
            "resumed": {"type": "integer", "minimum": 0},
            "lastFailure": {"type": "string"}
          }
        },
        "conditions": {"type": "array", "items": {"type": "object"}},
        "observedGeneration": {"type": "integer", "minimum": 0}
      }
    }
  }
}`

// nodeProviderKindSchema is the JSON Schema of node providers. A provider
// application writes its object when it starts and keeps the heartbeat in the
// status; credentials of the platform never enter an object.
const nodeProviderKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["endpoint"],
      "properties": {
        "endpoint": {"type": "string", "pattern": "^https?://.+"},
        "token_variable": {"type": "string"},
        "groups": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["name"],
            "properties": {
              "name": {"type": "string", "minLength": 1},
              "max": {"type": "integer", "minimum": 0},
              "parameters": {"type": "object"}
            },
            "additionalProperties": false
          }
        },
        "parameters": {"type": "object"}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "heartbeat": {"type": "string"},
        "version": {"type": "string"},
        "conditions": {"type": "array", "items": {"type": "object"}},
        "observedGeneration": {"type": "integer", "minimum": 0}
      }
    }
  }
}`

// builtinNodeScalingKinds returns the kinds of node groups and node
// providers.
func builtinNodeScalingKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{
		{
			Group:       structs.NodeScalingGroup,
			Kind:        structs.NodeGroupKind,
			Description: "Capacity group of nodes of a node pool in a datacenter, scaled by the autoscaler and created by an operator (built in)",
			Namespaced:  false,
			Schema:      nodeGroupKindSchema,
			Scale:       &structs.ObjectKindScale{SpecPath: "spec.replicas", StatusPath: "status.replicas"},
			Rules: []*structs.ObjectKindRule{
				{
					Expression: "!has(self.spec.min) || self.spec.min <= self.spec.max",
					Message:    "min must not exceed max",
				},
				{
					Expression: "!has(self.spec.replicas) || (self.spec.replicas <= self.spec.max && (!has(self.spec.min) || self.spec.replicas >= self.spec.min))",
					Message:    "replicas must be between min and max",
				},
				{
					Expression: "(has(self.spec.engine) && self.spec.engine == 'external') || (has(self.spec.provider) && self.spec.provider != '')",
					Message:    "provider is required unless the engine is external",
				},
				{
					Expression: "!has(oldSelf) || (self.spec.pool == oldSelf.spec.pool && self.spec.datacenter == oldSelf.spec.datacenter)",
					Message:    "pool and datacenter can't be changed: the nodes of the group would no longer belong to it",
				},
				{
					Expression: "!has(oldSelf) || (has(self.spec.node_class) ? self.spec.node_class : '') == (has(oldSelf.spec.node_class) ? oldSelf.spec.node_class : '')",
					Message:    "node_class can't be changed",
				},
			},
			Columns: []*structs.ObjectKindColumn{
				{Name: "Pool", Path: "spec.pool"},
				{Name: "Datacenter", Path: "spec.datacenter"},
				{Name: "Class", Path: "spec.node_class"},
				{Name: "Replicas", Path: "spec.replicas"},
				{Name: "Ready", Path: "status.replicas"},
				{Name: "Min", Path: "spec.min"},
				{Name: "Max", Path: "spec.max"},
				{Name: "Enforce", Path: "spec.enforce"},
			},
		},
		{
			Group:       structs.NodeScalingGroup,
			Kind:        structs.NodeProviderKind,
			Description: "Provider of nodes: an application that creates and deletes machines of a platform (built in)",
			Namespaced:  false,
			Schema:      nodeProviderKindSchema,
			Columns: []*structs.ObjectKindColumn{
				{Name: "Endpoint", Path: "spec.endpoint"},
				{Name: "Heartbeat", Path: "status.heartbeat"},
				{Name: "Version", Path: "status.version"},
			},
		},
	}
}

// isManagedObjectKind reports whether the kind is registered by the core and
// can't be changed or deleted through the object kind API. Its objects can
// be written through the generic object API.
func isManagedObjectKind(group, kind string) bool {
	if group != structs.NodeScalingGroup {
		return false
	}
	return kind == structs.NodeGroupKind || kind == structs.NodeProviderKind
}

// registerKinds registers the built-in kinds, or updates them if they
// differ from the built-in definitions. It runs on the leader.
func (s *Server) registerKinds(kinds []*structs.ObjectKind) error {
	if !ServersMeetMinimumNorcaVersion(s.serf.Members(), s.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later", minNorcaFeaturesVersion)
	}
	snap, err := s.State().Snapshot()
	if err != nil {
		return err
	}
	for _, kind := range kinds {
		existing, err := snap.ObjectKindByName(nil, kind.Group, kind.Kind)
		if err != nil {
			return err
		}
		if existing != nil && objectKindDefinitionEqual(existing, kind) {
			continue
		}
		if err := s.objects.compile(kind); err != nil {
			return fmt.Errorf("built-in kind %s does not compile: %v", kind.KindRef(), err)
		}
		req := &structs.ObjectKindUpsertRequest{
			ObjectKind:   kind,
			WriteRequest: structs.WriteRequest{Region: s.Region()},
		}
		if _, _, err := s.raftApply(structs.ObjectKindUpsertRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
			return err
		}
		s.objects.forget(kind.KindRef())
	}
	return nil
}

// nodeScalingKindsLoop registers the kinds of node groups and providers
// while this server is the leader, and tries again until it works.
func (s *Server) nodeScalingKindsLoop(stopCh chan struct{}) {
	for {
		err := s.registerKinds(builtinNodeScalingKinds())
		if err == nil {
			return
		}
		s.logger.Warn("registering the node scaling kinds failed, will retry", "error", err)
		select {
		case <-stopCh:
			return
		case <-time.After(nodeKindsRetry):
		}
	}
}

func errManagedObjectKind(group, kind string) error {
	return structs.NewErrRPCCodedf(http.StatusBadRequest,
		"the kind %s/%s is built in: its definition can't be changed or deleted", group, kind)
}
