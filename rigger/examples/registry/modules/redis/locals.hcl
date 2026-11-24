# The configuration file of the server: a template of the task ({{ }} is
# consul-template, filled in when the task starts). The line of the template
# language is written as a heredoc, where a backslash is the template's own.
locals {
  # nomad/jobs/<job id> is the path a job reads by its workload identity,
  # without a policy.
  auth_path = "nomad/jobs/${var.job_name}"

  # The password as a quoted value of redis.conf: a backslash and a double
  # quote in it are escaped. Without the Variable the task fails instead of
  # starting a server that asks for no password.
  auth_line = trimspace(<<-EOT
    requirepass "{{ with nomadVar "PATH" }}{{ .password.Value | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\"" }}{{ else }}{{ sprig_fail "the Variable PATH is missing: Redis is not started without its password" }}{{ end }}"
  EOT
  )

  config = join("\n", concat(
    [
      # The port of the group: var.port in bridge mode, the port the node
      # gives in host mode.
      "port {{ env \"NOMAD_PORT_db\" }}",
      "dir /data",
    ],
    # Without persistence nothing is written: no append-only file and no
    # snapshot into the layer of the container.
    var.persistence_enabled ? ["appendonly yes"] : ["appendonly no", "save \"\""],
    var.auth_enabled ? [replace(local.auth_line, "PATH", local.auth_path)] : [],
    [""],
  ))
}
