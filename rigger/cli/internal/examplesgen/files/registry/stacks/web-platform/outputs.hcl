# What the stack tells whoever applies it, and what a stack that takes it as
# a part reads as stack.<part>.<output>.

output "url" {
  value       = module.web.url
  description = "The address of the route; null without a host name."
}

output "service" {
  value       = module.web.service
  description = "The web service in the catalog."
}

output "cache" {
  value       = module.cache.connection
  description = "The connection of the cache, for more applications."
}
