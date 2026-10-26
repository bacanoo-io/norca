[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
[[- $job := $v.job_name ]]
[[- $ns := $v.namespace ]][[ if eq $ns "" ]][[ $ns = "<namespace of the release>" ]][[ end ]]
Test application "[[ $job ]]" in namespace [[ $ns ]], image [[ $v.image ]], network [[ $v.network_mode ]][[ if $v.mtls_sidecar ]] with an mTLS sidecar[[ end ]].
[[- if $v.secrets_enabled ]]

The task waits for the secret [[ $job ]]/db (items username and password):
  norca secret put -namespace <namespace> [[ $job ]]/db username=app password=-
[[- end ]]

Check it:
  norca job status -namespace <namespace> [[ $job ]]          # disruption budget, allocations
  norca service info -namespace <namespace> -verbose [[ $job ]]  # instances and check results
[[- if $v.scaling_enabled ]]
  norca scaling policy list -namespace <namespace> -job [[ $job ]]
[[- end ]]
