# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "oversubscription-exec" {
  datacenters = ["dc1"]

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "group" {
    task "task" {
      driver = "exec"

      config {
        command = "/bin/sh"
        args    = ["-c", "cat /proc/self/cgroup  | grep memory | cut -d: -f3 | tee ${NORCA_ALLOC_DIR}/tmp/cgroup_name; sleep 1000"]
      }

      resources {
        cpu        = 500
        memory     = 20
        memory_max = 30
      }
    }

    task "cgroup-fetcher" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args = ["-c", <<EOF
until [ -s "${NORCA_ALLOC_DIR}/tmp/cgroup_name" ]; do
  sleep 0.1
done

cat "/sys/fs/cgroup/memory/$(cat "${NORCA_ALLOC_DIR}/tmp/cgroup_name" )/memory.limit_in_bytes" \
  | tee "${NORCA_ALLOC_DIR}/tmp/memory.limit_in_bytes"

sleep 1000

EOF
        ]
      }

      resources {
        cpu    = 500
        memory = 20
      }
    }
  }
}
