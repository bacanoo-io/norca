# What the calling module reads as module.sizing.<output>.

output "cpu" {
  value       = local.size.cpu
  description = "CPU of an instance in MHz."
}

output "memory" {
  value       = local.size.memory
  description = "Memory reserved for an instance in MiB."
}

# null leaves the attribute out of a job: with a factor of 1 the instance
# has no limit above its reservation.
output "memory_max" {
  value       = var.memory_max_factor == 1 ? null : local.size.memory * var.memory_max_factor
  description = "Memory an instance may use above its reservation in MiB; null for none."
}
