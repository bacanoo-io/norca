output "ui_service" {
  value       = module.openobserve.service
  description = "Native service name of the OpenObserve UI."
}

output "otlp" {
  description = "Where applications send OTLP: the collector on their own node."
  value = {
    grpc = "${runtime_env("NORCA_HOST_IP")}:4317"
    http = "http://${runtime_env("NORCA_HOST_IP")}:4318"
  }
}
