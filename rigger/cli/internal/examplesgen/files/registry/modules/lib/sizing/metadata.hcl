# A library module: no resources, only outputs. Other modules of this
# registry call it as a child module (modules.hcl of webapp:
# source = "../lib/sizing"), so a rule they share is written once.
#
# A library is never installed and never published on its own: the module
# that calls it carries it. Its files are in the tree of the commit the tag
# of the calling module points to, and the digest of the calling module
# covers them.

module {
  name        = "sizing"
  version     = "1.0.0" # optional for a library; kept to say when it changes
  description = "The CPU and memory of an instance for a size: small, medium or large."
  library     = true # can't be installed on its own; only called by other modules
}
