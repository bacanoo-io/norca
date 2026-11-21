# One server that receives the changes of the Norca region as a leaf node of
# the system bus, into the account of the user audit.
#   norca secret put norca/bus/leaf token=<secret ID> ca=@/etc/norca.d/tls/ca.pem

count = 1

users = [
  {
    name            = "audit"
    account         = "AUDIT"
    subscribe_allow = ["norca.>", "_INBOX.>"]
    publish_allow   = ["$JS.norca-eu.API.CONSUMER.>", "$JS.ACK.>"]
  },
]

norca_bus = {
  servers = ["10.0.0.11:4652", "10.0.0.12:4652", "10.0.0.13:4652"]
  account = "AUDIT"
}
