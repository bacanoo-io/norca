# Clean room similarity audit

| Property | Value |
| --- | --- |
| norca_ref | `main` |
| norca_commit | `7b6609684a1e85089c885305abf1b7508a861439` |
| base_tag | `v1.6.5` |
| full_tag | `v2.1.1` |
| full_commit | `d0f21042997689c7690421ede8849be694e2ce46` |
| dolos | `2.10.0` |
| sets | `full` |
| threshold | `0.4` |
| date | `2026-11-11T23:56:29Z` |

Only pairs at or above the threshold (0.4) are listed, here and in pairs.csv; the counts cover every pair.

## full

1546 new Norca files against every file of the BSL release with the same language, whatever its path (Dolos reports pairs of at least 0.2). 2971862 cross pairs, 10355 at or above 0.4; 109 candidates for the auditor: Norca files at or above the threshold and above the MPL baseline of the BSL file (the highest similarity of a v1.6.5 file to it).

### Candidates

| Norca file | Kind | BSL file | Similarity | MPL baseline | Same path | Longest fragment | Total overlap |
| --- | --- | --- | --- | --- | --- | --- | --- |
| nomad/autoscaler/sdk/plugin.go | new | scheduler/structs/const.go | 1.000 | 0.658 | false | 5 | 21 |
| pack/internal/config/configNomad.go | new | nomad/structs/service_identities.go | 1.000 | 0.296 | false | 6 | 15 |
| nomad/autoscaler/sdk/plugin.go | new | command/agent/sdnotify.go | 1.000 | 0.667 | false | 3 | 10 |
| pack/terminal/input.go | new | nomad/structs/service_identities.go | 1.000 | 0.296 | false | 3 | 9 |
| plugins/drivers/fsisolation/fsisolation.go | new | plugins/drivers/fsisolation/isolation.go | 0.842 | 0.333 | false | 16 | 32 |
| nomad/structs/node_scaling.go | new | scheduler/structs/const.go | 0.840 | 0.658 | false | 4 | 21 |
| pack/internal/runner/job/cli.go | new | client/structs/host_volumes.go | 0.823 | 0.477 | false | 9 | 51 |
| ui/tests/integration/components/jobs/service-page-test.js | new | ui/tests/integration/components/job-page/service-test.js | 0.786 | 0.780 | false | 44 | 956 |
| command/network_policy_delete.go | new | command/job_tag_unset.go | 0.767 | 0.755 | false | 21 | 244 |
| command/operator_backup_inspect.go | new | command/job_tag_unset.go | 0.762 | 0.755 | false | 20 | 247 |
| pack/internal/runner/job/cli.go | new | nomad/structs/service_identities.go | 0.714 | 0.296 | false | 6 | 20 |
| nomad/structs/node_scaling.go | new | command/agent/sdnotify.go | 0.714 | 0.667 | false | 3 | 10 |
| pack/internal/config/configNomad.go | new | client/structs/host_volumes.go | 0.673 | 0.477 | false | 7 | 33 |
| drivers/exec2/pkg/util/unix_test.go | new | helper/subproc/self_test.go | 0.667 | 0.516 | false | 7 | 14 |
| command/admission_policy_list.go | new | command/queue_status.go | 0.663 | 0.657 | false | 18 | 333 |
| nomad/structs/autoscaler_usage.go | new | client/structs/host_volumes.go | 0.655 | 0.477 | false | 10 | 57 |
| api/secrets_delivery.go | new | client/allocrunner/cni/plugins.go | 0.653 | 0.414 | false | 12 | 62 |
| nomad/autoscaler/plugins/apm/nomadapm/job_test.go | new | nomad/structs/actions_test.go | 0.643 | 0.634 | false | 8 | 396 |
| pack/internal/runner/job/hcl_extract_test.go | new | nomad/structs/constraint_test.go | 0.638 | 0.594 | false | 9 | 148 |
| pack/terminal/input.go | new | client/structs/host_volumes.go | 0.628 | 0.477 | false | 3 | 27 |
| command/service_intention.go | new | command/job_start.go | 0.621 | 0.610 | false | 22 | 775 |
| api/server_role.go | new | nomad/structs/service_identities.go | 0.619 | 0.296 | false | 5 | 13 |
| command/admission_policy_list.go | new | command/queue_jobs.go | 0.619 | 0.613 | false | 30 | 406 |
| api/server_role.go | new | client/structs/host_volumes.go | 0.618 | 0.477 | false | 7 | 34 |
| nomad/structs/scaling_decision.go | new | client/structs/host_volumes.go | 0.613 | 0.477 | false | 7 | 38 |
| internal/tstorage/encoding_test.go | new | drivers/docker/util/util_test.go | 0.608 | 0.569 | false | 11 | 192 |
| nomad/structs/scaling_decision.go | new | nomad/structs/service_identities.go | 0.607 | 0.296 | false | 5 | 17 |
| internal/tstorage/disk_partition_test.go | new | command/agent/log_levels_test.go | 0.603 | 0.594 | false | 8 | 82 |
| command/agent/network_policy_endpoint_test.go | new | command/agent/batch_job_queue_endpoint_test.go | 0.595 | 0.586 | false | 14 | 226 |
| helper/diskusage/diskusage_other.go | new | helper/funcs_unix.go | 0.588 | 0.300 | false | 5 | 10 |
| drivers/exec2/pkg/util/unix_test.go | new | client/lib/cgroupslib/memory_test.go | 0.583 | 0.444 | false | 7 | 14 |
| pack/internal/runner/job/release.go | new | helper/subproc/self_test.go | 0.583 | 0.516 | false | 7 | 14 |
| nomad/structs/server_role.go | new | client/structs/host_volumes.go | 0.576 | 0.477 | false | 7 | 38 |
| internal/tstorage/fake_partition.go | new | nomad/structs/queue.go | 0.575 | 0.493 | false | 14 | 88 |
| drivers/exec2/pkg/util/io_test.go | new | client/lib/numalib/detect_test.go | 0.569 | 0.455 | false | 5 | 29 |
| pack/internal/pkg/spinner/character_sets.go | new | command/agent/log_levels.go | 0.550 | 0.271 | false | 4 | 463 |
| drivers/exec2/pkg/util/unix.go | new | helper/winsvc/privileged_windows.go | 0.545 | 0.400 | false | 3 | 6 |
| ui/app/components/ui/column-picker.js | new | ui/app/components/app-breadcrumbs.js | 0.545 | 0.533 | false | 6 | 12 |
| command/service_intention.go | new | command/operator_snapshot_inspect.go | 0.545 | 0.441 | false | 43 | 722 |
| nomad/state/state_store_quota.go | new | nomad/state/state_store_host_volumes.go | 0.544 | 0.539 | false | 27 | 904 |
| nomad/autoscaler/plugins/external/executable_windows.go | new | helper/funcs_windows.go | 0.538 | 0.522 | false | 3 | 14 |
| nomad/structs/csi_usage.go | new | client/structs/host_volumes.go | 0.530 | 0.477 | false | 7 | 44 |
| pack/internal/pkg/spinner/character_sets.go | new | nomad/structs/constraint.go | 0.526 | 0.257 | false | 3 | 462 |
| helper/raftbadger/store_test.go | new | helper/raftutil/migrate_test.go | 0.521 | 0.439 | false | 25 | 496 |
| ui/scripts/generate-icons.js | new | ui/testem.js | 0.520 | 0.500 | false | 4 | 106 |
| pack/internal/runner/job/release.go | new | client/lib/cgroupslib/memory_test.go | 0.519 | 0.444 | false | 7 | 14 |
| pack/internal/pkg/spinner/character_sets.go | new | lib/lang/maps_test.go | 0.513 | 0.286 | false | 3 | 462 |
| command/secret_engine.go | new | command/operator_snapshot_inspect.go | 0.512 | 0.441 | false | 23 | 865 |
| client/mtlsproxy/envoy_integration_test.go | new | helper/winsvc/windows_service_windows_test.go | 0.509 | 0.463 | false | 11 | 1420 |
| pack/internal/pkg/helper/filesystem/filesystem_test.go | new | helper/escapingfs/copydir_test.go | 0.506 | 0.387 | false | 13 | 128 |
| nomad/job_endpoint_hook_vault_namespace_test.go | new | nomad/job_endpoint_hook_vault_ce_test.go | 0.503 | 0.427 | false | 10 | 165 |
| drivers/exec2/pkg/task/store_test.go | new | nomad/job_endpoint_hook_numa_ce_test.go | 0.500 | 0.480 | false | 8 | 54 |
| api/operator_seal.go | new | api/batch_job_queue.go | 0.497 | 0.480 | false | 11 | 161 |
| nomad/structs/metrics_store.go | new | client/structs/host_volumes.go | 0.493 | 0.477 | false | 7 | 73 |
| pack/internal/pkg/spinner/character_sets.go | new | client/lib/cgroupslib/mount.go | 0.493 | 0.265 | false | 3 | 462 |
| pack/internal/pkg/spinner/character_sets.go | new | nomad/util_test.go | 0.492 | 0.454 | false | 4 | 475 |
| ui/scripts/generate-icons.js | new | ui/app/utils/wrapped-fetch.js | 0.492 | 0.274 | false | 1 | 94 |
| pack/internal/pkg/spinner/character_sets.go | new | client/config/fingerprint.go | 0.492 | 0.443 | false | 2 | 461 |
| nomad/structs/autoscaler_usage.go | new | nomad/structs/service_identities.go | 0.491 | 0.296 | false | 5 | 26 |
| pack/internal/pkg/spinner/character_sets.go | new | nomad/lock/delay.go | 0.488 | 0.383 | false | 2 | 461 |
| nomad/event_endpoint_variables_test.go | new | helper/winsvc/windows_service_windows_test.go | 0.484 | 0.463 | false | 11 | 1080 |
| command/agent/config_bus.go | new | nomad/structs/config/reporting.go | 0.483 | 0.480 | false | 13 | 290 |
| nomad/structs/secret_engine.go | new | nomad/structs/host_volumes.go | 0.483 | 0.430 | false | 16 | 556 |
| command/agent/http_h2_test.go | new | helper/winsvc/windows_service_windows_test.go | 0.482 | 0.463 | false | 11 | 974 |
| pack/internal/pkg/errors/ui_context_test.go | new | nomad/structs/connect_test.go | 0.479 | 0.474 | false | 7 | 173 |
| pack/internal/cli/cli_v1_test.go | new | client/allocrunner/taskrunner/getter/sandbox_test.go | 0.479 | 0.468 | false | 17 | 2122 |
| helper/users/dynamic/dynamic_test.go | new | helper/users/dynamic/pool_test.go | 0.479 | 0.438 | false | 23 | 271 |
| pack/internal/pkg/loader/loader_test.go | new | helper/winsvc/windows_service_windows_test.go | 0.477 | 0.463 | false | 9 | 1019 |
| command/agent/mtls_endpoint.go | new | command/agent/job_endpoint_statuses.go | 0.475 | 0.420 | false | 9 | 260 |
| helper/diskusage/diskusage_test.go | new | helper/escapingfs/copydir_test.go | 0.474 | 0.387 | false | 18 | 100 |
| pack/internal/pkg/spinner/character_sets.go | new | nomad/lock/ttl.go | 0.473 | 0.412 | false | 2 | 461 |
| command/object_kind.go | new | command/operator_snapshot_inspect.go | 0.471 | 0.441 | false | 22 | 779 |
| command/service_intention.go | new | command/operator_autopilot_health.go | 0.470 | 0.425 | false | 30 | 609 |
| nomad/structs/csi_usage.go | new | nomad/structs/service_identities.go | 0.469 | 0.296 | false | 5 | 23 |
| pack/internal/pkg/spinner/character_sets.go | new | e2e/allocexec/docker_exec_test.go | 0.468 | 0.397 | false | 2 | 461 |
| command/operator_backup_restore_bootstrap.go | new | command/windows_service_install.go | 0.467 | 0.419 | false | 15 | 706 |
| command/object_kind_ecosystem.go | new | command/operator_snapshot_inspect.go | 0.466 | 0.441 | false | 22 | 399 |
| command/operator_metrics_store.go | new | command/operator_snapshot_inspect.go | 0.458 | 0.441 | false | 21 | 361 |
| command/secret_engine.go | new | command/windows_service_install.go | 0.457 | 0.419 | false | 18 | 873 |
| command/secret_engine.go | new | command/setup_vault.go | 0.456 | 0.413 | false | 20 | 1179 |
| nomad/structs/backup.go | new | nomad/structs/host_volumes.go | 0.455 | 0.430 | false | 21 | 575 |
| command/secret_engine.go | new | command/operator_autopilot_health.go | 0.452 | 0.425 | false | 19 | 751 |
| command/operator_consul_kv_test.go | new | command/node_identity_get_test.go | 0.451 | 0.419 | false | 12 | 192 |
| command/object.go | new | command/operator_snapshot_inspect.go | 0.451 | 0.441 | false | 20 | 812 |
| command/operator_bus_status_test.go | new | command/node_identity_get_test.go | 0.450 | 0.419 | false | 10 | 162 |
| nomad/autoscaler/sdk/plugin.go | new | helper/winsvc/service.go | 0.450 | 0.405 | false | 4 | 9 |
| pack/internal/pkg/spinner/character_sets.go | new | command/agent/tls_metrics.go | 0.449 | 0.403 | false | 2 | 463 |
| pack/internal/pkg/deps/vendor_oci_test.go | new | helper/raftutil/migrate_test.go | 0.448 | 0.439 | false | 12 | 269 |
| command/operator_backup_restore_bootstrap.go | new | command/setup_vault.go | 0.446 | 0.413 | false | 13 | 974 |
| command/deployment_run_test.go | new | command/node_identity_get_test.go | 0.446 | 0.419 | false | 16 | 193 |
| command/service_intention.go | new | command/setup_vault.go | 0.443 | 0.413 | false | 28 | 983 |
| internal/tstorage/wal.go | new | nomad/leader_ce.go | 0.443 | 0.435 | false | 5 | 31 |
| pack/internal/pkg/packlock/lock_test.go | new | helper/escapingfs/copydir_test.go | 0.442 | 0.387 | false | 11 | 200 |
| client/allocrunner/checks_hook_thresholds_test.go | new | helper/safemath/safemath_test.go | 0.441 | 0.370 | false | 12 | 64 |
| pack/internal/pkg/spinner/character_sets.go | new | client/allocrunner/networking_iptables_test.go | 0.438 | 0.320 | false | 4 | 465 |
| command/object.go | new | command/setup_vault.go | 0.434 | 0.413 | false | 22 | 1169 |
| command/secret_engine.go | new | command/var_lock.go | 0.427 | 0.371 | false | 17 | 808 |
| command/admission_policy_variable_test.go | new | command/node_identity_get_test.go | 0.426 | 0.419 | false | 13 | 196 |
| command/object_kind.go | new | command/setup_vault.go | 0.426 | 0.413 | false | 20 | 1085 |
| drivers/exec2/pkg/util/io_test.go | new | lib/lang/stack_test.go | 0.424 | 0.416 | false | 4 | 36 |
| command/object.go | new | command/windows_service_install.go | 0.420 | 0.419 | false | 18 | 850 |
| internal/tstorage/wal.go | new | client/lib/proclib/testing.go | 0.419 | 0.348 | false | 12 | 44 |
| nomad/structs/backup.go | new | nomad/structs/deployment.go | 0.418 | 0.352 | false | 24 | 451 |
| nomad/check_roles_test.go | new | nomad/locks_test.go | 0.417 | 0.401 | false | 14 | 361 |
| command/service_intention.go | new | command/var_lock.go | 0.417 | 0.371 | false | 15 | 636 |
| nomad/job_endpoint_hook_alloc_affinity_test.go | new | nomad/locks_test.go | 0.408 | 0.401 | false | 13 | 313 |
| command/secret_engine.go | new | command/setup_consul.go | 0.407 | 0.376 | false | 20 | 1053 |
| nomad/structs/server_role.go | new | nomad/structs/service_identities.go | 0.406 | 0.296 | false | 5 | 13 |
| pack/internal/pkg/spinner/character_sets.go | new | client/allocrunner/max_run_duration_hook.go | 0.401 | 0.325 | false | 2 | 463 |

### Other pairs at or above the threshold

Not candidates: HashiCorp's own MPL code resembles the BSL file at least as much. The 50 highest; pairs.csv has all of them.

| Norca file | Kind | BSL file | Similarity | MPL baseline | Same path |
| --- | --- | --- | --- | --- | --- |
| command/operator_bus.go | new | command/job_periodic.go | 1.000 | 1.000 | false |
| command/operator_bus.go | new | command/job_tag.go | 1.000 | 1.000 | false |
| command/operator_bus.go | new | command/node_intro.go | 1.000 | 1.000 | false |
| command/operator_bus.go | new | command/operator_autopilot.go | 1.000 | 1.000 | false |
| command/operator_bus.go | new | command/volume_snapshot.go | 1.000 | 1.000 | false |
| nomad/autoscaler/plugins/external/executable_unix.go | new | helper/pluginutils/loader/filter_unix.go | 1.000 | 1.000 | false |
| ui/tests/pages/components/alert.js | new | ui/tests/pages/components/notification.js | 1.000 | 1.000 | false |
| ui/app/routes/jobs/job/events.js | new | ui/app/routes/variables/variable/edit.js | 1.000 | 1.000 | false |
| ui/app/routes/jobs/job/placement.js | new | ui/app/routes/variables/variable/edit.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/controllers/administration/sentinel-policies.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/controllers/allocations/allocation/fs-root.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/controllers/allocations/allocation/task/fs-root.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/controllers/jobs.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/controllers/jobs/job/dispatch.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/controllers/jobs/job/services.js | 1.000 | 1.000 | false |
| ui/app/routes/endpoints.js | new | ui/app/routes/jobs/job/services/index.js | 1.000 | 1.000 | false |
| command/deployment_run.go | new | command/deployment_unblock.go | 0.978 | 1.000 | false |
| command/deployment_run.go | new | command/deployment_resume.go | 0.964 | 1.000 | false |
| command/admission_policy_list.go | new | command/node_pool_list.go | 0.957 | 0.990 | false |
| command/deployment_run.go | new | command/deployment_fail.go | 0.956 | 1.000 | false |
| ui/tests/pages/components/switch.js | new | ui/tests/pages/components/two-step-button.js | 0.943 | 1.000 | false |
| ui/tests/integration/components/jobs/task-groups-table-test.js | new | ui/tests/integration/components/job-page/parts/task-groups-test.js | 0.918 | 0.926 | false |
| ui/tests/pages/backups.js | new | ui/tests/pages/settings/tokens.js | 0.917 | 1.000 | false |
| ui/tests/pages/components/switch.js | new | ui/tests/pages/components/toggle.js | 0.909 | 1.000 | false |
| command/operator_bus.go | new | command/acl.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/alloc.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/config.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/deployment.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/eval.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/node_identity.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/node_meta.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/operator.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/operator_raft.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/operator_snapshot.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/sentinel.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/server.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/service.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/setup.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/system.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/system_reconcile.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/volume.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/windows.go | 0.906 | 1.000 | false |
| command/operator_bus.go | new | command/windows_service.go | 0.906 | 1.000 | false |
| command/deployment_run.go | new | command/deployment_pause.go | 0.904 | 1.000 | false |
| command/admission_policy_list.go | new | command/node_pool_jobs.go | 0.889 | 1.000 | false |
| command/deployment_run.go | new | command/deployment_promote.go | 0.889 | 1.000 | false |
| command/admission_policy_delete.go | new | command/quota_delete.go | 0.881 | 1.000 | false |
| ui/app/controllers/objects/index.js | new | ui/app/controllers/jobs/run.js | 0.880 | 1.000 | false |
| command/agent/network_policy_endpoint.go | new | command/agent/node_pool_endpoint.go | 0.879 | 0.996 | false |
| ui/tests/integration/components/jobs/placement-failures-test.js | new | ui/tests/integration/components/job-page/parts/placement-failures-test.js | 0.874 | 0.913 | false |

## fullmod

The lines Norca added to 1188 changed files (17675 lines; module path and Norca header lines aside) against the lines HashiCorp added to the same file between the base tag and the BSL release, character by character. 94 files the BSL release no longer has at their path were compared with its files of the same name. Only the pairs of a file with its own BSL file count: 1542 cross pairs, 7 at or above 0.4; 3 candidates for the auditor (at or above the threshold with a common fragment of at least 10 k-grams). 4 pairs at or above the threshold share only shorter fragments (an import line, a brace): pairs.csv lists them.

| Norca file (added lines) | BSL file (added lines) | Similarity | Same path | Longest fragment | Total overlap |
| --- | --- | --- | --- | --- | --- |
| api/deployments_test.go | api/deployments_test.go | 0.878 | true | 36 | 72 |
| nomad/structs/config/tls_test.go | nomad/structs/config/tls_test.go | 0.466 | true | 26 | 88 |
| ui/tests/integration/components/task-log-test.js | ui/tests/integration/components/task-log-test.js | 0.412 | true | 23 | 310 |
