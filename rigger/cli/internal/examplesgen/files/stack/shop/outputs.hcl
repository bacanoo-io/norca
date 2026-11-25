# Shown after an apply and by rigger stack status.

output "url" {
  value       = module.web.url
  description = "The address of the web service; null without a route."
}

output "web_service" {
  value = module.web.service
}

output "cache_service" {
  value = module.cache.service
}
