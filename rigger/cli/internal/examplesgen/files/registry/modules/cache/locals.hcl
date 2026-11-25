locals {
  name = rigger.release

  # The configuration of Valkey, built from data. The password is a
  # reference of run time: secret_ref() writes the task template that reads
  # it from the secret store when the task starts, quoted for the
  # configuration file ("conf": in double quotes, \ and " escaped).
  config = join("\n", concat(
    [
      "port ${var.port}",
      "dir /data",
      "maxmemory ${floor(var.memory * 0.8)}mb",
      "maxmemory-policy ${var.eviction}",
      "requirepass ${secret_ref(var.password_secret, "password", { quote = "conf" })}",
    ],
    var.persistence ? ["appendonly yes"] : ["appendonly no", "save \"\""],
    [""],
  ))
}
