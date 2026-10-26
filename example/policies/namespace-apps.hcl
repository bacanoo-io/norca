# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Namespace of the example applications:
#   norca namespace apply namespace-apps.hcl
name        = "apps"
description = "Example applications"
quota       = "apps"

meta {
  # Pack releases in production namespaces are atomic: a failed install or
  # upgrade restores the previous revision.
  environment = "production"
  team        = "demo"
}
