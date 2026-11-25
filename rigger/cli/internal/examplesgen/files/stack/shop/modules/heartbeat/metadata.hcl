# A module of the stack itself: it lives next to stack.hcl, has no registry
# and no tag, and needs no version: its version is the commit of this
# repository. Everything else is a module as any other.

module {
  name        = "heartbeat"
  description = "A periodic job that asks the web service for its health and counts the answers in the cache."
  category    = "application"
}
