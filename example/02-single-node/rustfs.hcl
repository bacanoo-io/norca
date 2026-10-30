# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the rustfs pack (Norca pack registry): one RustFS server as
# the backup store of this node, its data on the host volume "rustfs", the
# backup bucket created by a poststart task. For this example only: in
# production keep the backups outside the cluster they protect.
#   norca secret put rustfs/root access_key=norca-backup secret_key=-
#   norca pack run rustfs --registry=norca --name=rustfs -f rustfs.hcl
buckets         = ["norca-backup"]
console_enabled = true
