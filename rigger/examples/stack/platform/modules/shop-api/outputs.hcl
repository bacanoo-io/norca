output "service" {
  value = local.name
}

# Passed on to the worker by the stack.
output "bus" {
  value = var.bus
}

output "order_subjects" {
  value = local.order_subjects
}
