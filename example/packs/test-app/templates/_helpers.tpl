[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]

[[- /* Checks of combinations of variables that the JSON Schema can't express. */ -]]
[[- define "test_app.validate" -]]
[[- $v := vars . -]]
[[- if and $v.mtls_sidecar (ne $v.network_mode "bridge") -]]
[[- fail "mtls_sidecar needs network_mode = \"bridge\"" -]]
[[- end -]]
[[- if and $v.multiregion (ne $v.region "") -]]
[[- fail "a multiregion job must not set region: every region of the multiregion block gets its copy" -]]
[[- end -]]
[[- if and $v.multiregion $v.scaling_enabled -]]
[[- fail "set scaling_enabled = false with multiregion: the group count is 0 and every region uses the count of its multiregion region" -]]
[[- end -]]
[[- if and $v.scaling_enabled (or (lt $v.count $v.scaling_min) (gt $v.count $v.scaling_max)) -]]
[[- fail "count must be between scaling_min and scaling_max" -]]
[[- end -]]
[[- end -]]

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
