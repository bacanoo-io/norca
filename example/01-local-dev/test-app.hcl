# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the test-app pack on a development agent:
#   norca pack run ../packs/test-app -f test-app.hcl
count = 1

# Docker port mapping, so the agent needs neither root nor the CNI plugins.
# Use "bridge" when the agent runs as root with the CNI plugins installed.
network_mode = "host"

discovery_prefer = "datacenter"
