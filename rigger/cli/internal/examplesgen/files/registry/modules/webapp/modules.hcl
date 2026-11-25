# Child modules: evaluated into the same release. The source is a path
# relative to this module, inside the same repository; the other
# attributes are values of the child's variables. The module reads the
# child's outputs as module.<name>.<output>.

module "sizing" {
  source = "../lib/sizing"

  size              = var.size
  memory_max_factor = var.memory_oversubscription ? 2 : 1
}
