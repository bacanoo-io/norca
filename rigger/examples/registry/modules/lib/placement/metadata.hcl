# A child module without resources: other modules of the registry call it
# for what they would otherwise each write again (where a job runs, the
# reference of an image). Replaces the helpers copied between packs.

module {
  name        = "placement"
  version     = "1.0.0"
  description = "Shared values of the modules of this registry: placement of a job and the reference of an image."
  library     = true # can't be installed on its own
}
