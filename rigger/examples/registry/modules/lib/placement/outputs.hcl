output "datacenters" {
  value = var.datacenters
}

# null leaves the attribute out of the job.
output "node_pool" {
  value = var.node_pool == "" ? null : var.node_pool
}

# A list of zero or one: fits the for_each of a dynamic "spread" block.
output "spread" {
  value = var.spread_over == "" ? [] : [{
    attribute = local.spread_attribute[var.spread_over]
    weight    = 100
  }]
}

output "image" {
  value = var.image_digest == "" ? var.image : "${var.image}@${var.image_digest}"
}
