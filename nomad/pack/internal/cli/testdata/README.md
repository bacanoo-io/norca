<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Test packs

`packs/consul` and `packs/loadbalancer-operator` are copies of the packs of
the [Norca pack registry](https://github.com/bacanoo-io/norca-registry), the
canonical source of the operations packs, without their READMEs. They are
used by the pack CLI tests of this directory (`consul_pack_test.go`,
`loadbalancer_operator_pack_test.go`) and by the end-to-end test of the
LoadBalancer operator (`nomad/controller/examples/loadbalancer/e2e_test.go`).

Change a pack in the registry first, then copy the changed files here
(`packs/<name>` of the registry, without `README.md`), so that the tests keep
testing what users install.
