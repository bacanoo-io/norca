# Clean room similarity audit

| Property | Value |
| --- | --- |
| norca_ref | `main` |
| norca_commit | `e98a2f58b22603b00458ef7e2002ff67d0b14ade` |
| base_tag | `v1.6.5` |
| bsl_tag | `v1.11.3` |
| bsl_commit | `173ab08a0210789da531847c4ce3c3518f7fb34b` |
| dolos | `2.10.0` |
| sets | `busl,bugfix` |
| threshold | `0.4` |
| date | `2026-11-02T15:56:23Z` |

## busl

18906 cross pairs, 103 at or above 0.4; 0 candidates for the auditor: Norca-added files at or above the threshold and above the MPL baseline (the highest similarity of HashiCorp's own MPL files to the same BUSL file).

### Candidates

| Norca file | BUSL file | Similarity | MPL baseline | Character level | Longest fragment | Total overlap |
| --- | --- | --- | --- | --- | --- | --- |
| – | – | – | – | – | – | – |

### Restored MPL files

Not in the v1.6.5 tree, but the MPL anchor (the import's second parent) has the same path, and the Norca file differs from it in at most 10% of the lines once the module path and the Norca header lines are removed. A BUSL file that grew out of the MPL file resembles it; these are not candidates.

| Norca file | Highest BUSL pair | Similarity | Lines | Changed lines |
| --- | --- | --- | --- | --- |
| e2e/rescheduling/rescheduling.go | e2e/rescheduling/rescheduling_test.go | 0.497 | 503 | 9 |

### Same-path pairs (clean room replacements)

| File | Similarity | MPL baseline | Character level | Longest fragment |
| --- | --- | --- | --- | --- |
| nomad/state/iterator_test.go | 0.307 | 0.411 | 0.065 | 7 |
| helper/backoff.go | 0.232 | 0.119 | 0.113 | 8 |
| client/structs/csi_test.go | 0.226 | 0.346 | 0.019 | 8 |
| ui/app/components/role-editor.js | 0.210 | 0.609 | 0.181 | 5 |
| ui/app/controllers/access-control/tokens/token.js | 0.186 | 0.426 | 0.141 | 4 |
| ui/app/abilities/role.js | 0.182 | 1.000 | 0.096 | 2 |
| ui/app/controllers/access-control/roles/index.js | 0.152 | 0.243 | 0.094 | 6 |
| testutil/mock_calls.go | 0.149 | 0.420 | 0.105 | 3 |
| ui/app/components/token-editor.js | 0.148 | 0.288 | 0.080 | 5 |
| ui/app/controllers/access-control/roles/role.js | 0.144 | 0.832 | 0.061 | 7 |
| ui/app/templates/access-control/tokens/new.hbs | 0.133 | 0.264 | 0.133 | 2 |
| ui/app/controllers/access-control/policies/index.js | 0.130 | 0.334 | 0.091 | 6 |
| ui/app/utils/json-to-hcl.js | 0.099 | 0.054 | 0.056 | 3 |
| ui/app/templates/access-control/policies/index.hbs | 0.096 | 0.052 | 0.096 | 7 |
| ui/mirage/models/token.js | 0.091 | 1.000 | 0.091 | 1 |
| ui/app/controllers/access-control/tokens/index.js | 0.090 | 0.304 | 0.068 | 3 |
| helper/backoff_test.go | 0.090 | 0.392 | 0.004 | 8 |
| ui/app/templates/access-control/roles/new.hbs | 0.078 | 0.184 | 0.078 | 1 |
| ui/app/templates/access-control/roles/index.hbs | 0.069 | 0.061 | 0.069 | 6 |
| ui/app/templates/access-control/roles.hbs | 0.069 | 0.500 | 0.069 | 1 |
| ui/app/templates/access-control/policies.hbs | 0.067 | 0.483 | 0.067 | 1 |
| ui/app/templates/access-control/tokens.hbs | 0.067 | 0.483 | 0.067 | 1 |
| ui/app/components/job-editor/review.js | 0.067 | 0.333 | 0.088 | 1 |
| ui/app/templates/access-control/tokens/index.hbs | 0.065 | 0.079 | 0.065 | 7 |
| ui/mirage/serializers/role.js | 0.060 | 0.764 | 0.108 | 2 |
| ui/app/templates/access-control/roles/role.hbs | 0.058 | 0.122 | 0.058 | 3 |
| ui/app/templates/access-control.hbs | 0.049 | 0.368 | 0.049 | 1 |
| ui/app/templates/access-control/tokens/token.hbs | 0.031 | 0.189 | 0.031 | 2 |
| ui/app/templates/access-control/index.hbs | 0.030 | 0.051 | 0.030 | 2 |
| ui/app/components/token-editor.hbs | 0.019 | 0.044 | 0.019 | 3 |
| ui/app/components/role-editor.hbs | 0.015 | 0.067 | 0.015 | 1 |
| ui/app/routes/access-control.js | 0.000 | 0.222 | 0.008 | 0 |
| ui/app/routes/access-control/roles/new.js | 0.000 | 0.427 | 0.018 | 0 |
| ui/app/routes/access-control/roles/role.js | 0.000 | 0.571 | 0.015 | 0 |
| ui/app/routes/access-control/tokens/new.js | 0.000 | 0.436 | 0.017 | 0 |
| ui/app/routes/access-control/tokens/token.js | 0.000 | 0.375 | 0.013 | 0 |

## bugfix

1221 cross pairs, 2 at or above 0.4; same-path pairs: 37.

| Norca commit and file | HashiCorp file | Similarity | Longest fragment | Total overlap |
| --- | --- | --- | --- | --- |
| 77aa0af7e00e nomad/structs/structs.generated.go | nomad/structs/structs.generated.go | 0.507 | 361 | 201544 |
| 32e5db0980b2 nomad/structs/structs.generated.go | nomad/structs/structs.generated.go | 0.427 | 346 | 168467 |
| 32e5db0980b2 command/commands.go | command/commands.go | 0.261 | 3 | 121 |
| 8aa389f276a1 nomad/structs/structs.generated.go | nomad/structs/structs.generated.go | 0.226 | 138 | 88552 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | nomad/node_endpoint_test.go | 0.093 | 9 | 792 |
| cc4d34a8fec4 plugins/drivers/client.go | plugins/drivers/client.go | 0.070 | 5 | 22 |
| 4e4b1eaa35e4 nomad/drainer_int_test.go | nomad/drainer_int_test.go | 0.037 | 3 | 9 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | nomad/heartbeat_test.go | 0.033 | 4 | 29 |
| 4e4b1eaa35e4 nomad/drainer_int_test.go | nomad/node_endpoint_test.go | 0.028 | 5 | 224 |
| 4e4b1eaa35e4 nomad/heartbeat_test.go | plugins/shared/structs/attribute_test.go | 0.025 | 1 | 2 |
| d2529f1c96ba nomad/volumewatcher/volumes_watcher_test.go | nomad/volumewatcher/volumes_watcher_test.go | 0.023 | 3 | 28 |
| 4e4b1eaa35e4 nomad/heartbeat_test.go | nomad/heartbeat_test.go | 0.022 | 1 | 8 |
| 4e4b1eaa35e4 nomad/heartbeat.go | nomad/heartbeat.go | 0.020 | 1 | 2 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | nomad/drainer_int_test.go | 0.018 | 3 | 14 |
| d25baac9831c drivers/docker/coordinator.go | drivers/docker/coordinator.go | 0.018 | 2 | 10 |
| 4e4b1eaa35e4 nomad/heartbeat_test.go | drivers/docker/driver_linux_test.go | 0.017 | 2 | 5 |
| 4e4b1eaa35e4 nomad/node_endpoint.go | nomad/node_endpoint.go | 0.016 | 4 | 32 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | plugins/csi/client_test.go | 0.016 | 2 | 13 |
| 4e4b1eaa35e4 nomad/heartbeat_test.go | nomad/node_endpoint_test.go | 0.013 | 4 | 106 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | nomad/volumewatcher/volumes_watcher_test.go | 0.011 | 2 | 11 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | plugins/shared/structs/attribute_test.go | 0.010 | 2 | 6 |
| 32e5db0980b2 command/agent/http.go | command/agent/http.go | 0.009 | 1 | 3 |
| 32e5db0980b2 command/commands.go | command/agent/http.go | 0.008 | 1 | 3 |
| 4e4b1eaa35e4 nomad/node_endpoint_test.go | drivers/docker/coordinator_test.go | 0.008 | 2 | 9 |
| d2529f1c96ba nomad/volumewatcher/volumes_watcher_test.go | nomad/heartbeat_test.go | 0.008 | 2 | 8 |
