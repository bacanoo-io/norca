# Three servers, two accounts, TLS.
#   norca secret put nats/users/shop password=-
#   norca secret put nats/users/audit password=-
#   norca secret put nats/tls ca=@ca.pem cert=@cert.pem key=@key.pem

count      = 3
tls_secret = "nats/tls"

users = [
  {
    name            = "shop"
    publish_allow   = ["shop.>", "$JS.API.>"]
    subscribe_allow = ["shop.>", "_INBOX.>"]
  },
  {
    name            = "audit"
    account         = "AUDIT"
    subscribe_allow = ["norca.>", "_INBOX.>"]
    publish_allow   = ["$JS.norca-eu.API.CONSUMER.>", "$JS.ACK.>"]
  },
]
