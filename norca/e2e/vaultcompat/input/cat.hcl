# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "cat" {
  type = "batch"
  group "testcase" {
    task "cat" {
      driver = "raw_exec"

      config {
        command = "cat"
        args    = ["${NORCA_SECRETS_DIR}/vault_token"]
      }

      vault {
        policies = ["default"]
      }
    }

    restart {
      attempts = 0
      mode     = "fail"
    }
  }
}
