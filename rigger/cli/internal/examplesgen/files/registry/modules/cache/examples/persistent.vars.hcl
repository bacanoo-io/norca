# A cache that keeps its data on the host volume "cache" of a node.
#   norca secret put cache/password password=-

persistence = true
host_volume = "cache"
memory      = 512
eviction    = "noeviction"
