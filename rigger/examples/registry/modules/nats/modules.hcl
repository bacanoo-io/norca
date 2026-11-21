module "placement" {
  source = "../lib/placement"

  datacenters  = var.datacenters
  node_pool    = var.node_pool
  spread_over  = local.clustered ? "datacenter" : ""
  image        = var.image
  image_digest = var.image_digest
}
