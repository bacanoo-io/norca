# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Values of the rustfs module (norca/rustfs of the global registry): one
# RustFS server as the backup store of this node, its data on the host
# volume "rustfs", the backup bucket created by a poststart task. For this
# example only: in production keep the backups outside the cluster they
# protect. The install of step 7 of the README sends these values; from a
# checkout of the registry they are a values file:
#   norca secret put rustfs/root access_key=norca-backup secret_key=-
#   norca rigger install <checkout>/modules/rustfs -name rustfs -f rustfs.hcl
buckets         = ["norca-backup"]
console_enabled = true
