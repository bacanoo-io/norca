# What is audited in the namespace of the shop. A declaration: it names
# resources and actions, never subjects, streams, consumers or tokens.
# The names of the attributes are not designed yet; they show the form.
audit "shop" {
  namespaces = [rigger.namespace]
  record     = ["job", "secret", "variable", "acl"]
  actions    = ["create", "update", "delete"]
  keep       = "90d"
  readers    = ["shop-auditor"] # ACL roles that may read the trail
}
