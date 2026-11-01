[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]

[[- /* One HTTP check of the service. Arguments: the role, the path, the
interval and whether the check addresses the allocation (sidecar mode). */ -]]
[[- define "test_app.check" -]]
      check {
        name     = [[ index . 0 | quote ]]
        role     = [[ index . 0 | quote ]]
        type     = "http"
        path     = [[ index . 1 | quote ]]
        interval = [[ index . 2 | quote ]]
        timeout  = "2s"
[[- if index . 3 ]]
        # The application port is not mapped to the host in sidecar mode:
        # the client checks it on the address of the allocation.
        address_mode = "alloc"
[[- end ]]
[[- if eq (index . 0) "startup" ]]
        startup_timeout = [[ index . 4 | quote ]]
[[- end ]]
      }
[[- end -]]
