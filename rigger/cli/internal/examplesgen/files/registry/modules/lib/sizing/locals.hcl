locals {
  # CPU in MHz, memory in MiB, by size.
  sizes = {
    small  = { cpu = 100, memory = 64 }
    medium = { cpu = 500, memory = 256 }
    large  = { cpu = 2000, memory = 1024 }
  }

  size = local.sizes[var.size]
}
