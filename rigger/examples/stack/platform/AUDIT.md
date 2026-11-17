# Audit: what it takes

## What the stack declares

The base of audit is a module of rig (`builtin/audit`, an idea, not built),
imported in `stack.hcl` like the other modules. What is audited is a
resource file of a module, `<name>.audit.hcl`, here
`modules/shop-api/shop.audit.hcl`; it says what and nothing about how:

```hcl
audit "shop" {
  namespaces = [rig.namespace]
  record     = ["job", "secret", "variable", "acl"]
  actions    = ["create", "update", "delete"]
  keep       = "90d"
  readers    = ["shop-auditor"]
}
```

The trail then holds records (when, who, which action, which resource, the
outcome), and a reader asks for the records of a resource, a person or a
time span. Once rig is embedded in the Norca servers, the module sits on
the internal bus and this declaration is all there is.

## Underneath, until rig is embedded

This part is the plumbing, and not what an author of a stack should have to
write. Until rig is embedded, the changes of the region are read from
outside as events of the system bus; these are the steps for it, with the
NATS of this stack as the place they arrive.

The data travels one way:

```
Norca servers (system bus) ──leaf node──> NATS of the stack (account AUDIT) ──> a reader
```

Only the NATS holds a Norca token. A reader uses a NATS user and never sees
the token.

### Steps

1. **The servers listen for leaf nodes.** In the agent configuration of the
   servers, or with norca-deployer `bus.leafnodes: true`:

   ```hcl
   server {
     bus {
       leafnodes {
         enabled = true
       }
     }
   }
   ```

   The port is `bus_leafnode`, 4652; open it to the nodes that run the NATS.

2. **A policy that allows a leaf and names what it may receive.** For the
   changes of the namespace `shop` in region `eu`:

   ```hcl
   cdc {
     receive = ["norca.eu.shop.>"]
     leaf    = true
   }
   ```

   ```shell
   norca acl policy apply -description "audit leaf of the shop" shop-audit-leaf policy.hcl
   ```

   The admin events (ACL changes, nodes, the audit of the secret store) need
   their right in addition: a management token receives all of them, another
   token only the topics it may read.

3. **A token of the policy.** Keep its accessor ID: the servers name the
   consumers after it.

   ```shell
   norca acl token create -name shop-audit-leaf -policy shop-audit-leaf
   ```

4. **The secret the NATS reads**: the secret ID of the token and the CA of
   the Norca servers.

   ```shell
   norca secret put -namespace shop norca/bus/leaf token=<secret ID> ca=@/etc/norca.d/tls/ca.pem
   ```

5. **The NATS attaches.** In this stack: set `bus_servers` (see
   `values/prod.vars.hcl`); the `nats` module then connects to the servers
   as a leaf node and the changes arrive in the account `AUDIT`.

   ```shell
   rig stack apply -f values/prod.vars.hcl
   norca operator bus status        # Leaf nodes = 1
   ```

6. **A reader connects to the NATS** as the user `audit` (password in the
   secret `nats/users/audit`, created by the stack) and reads in two ways:

   - **live:** subscribe to `norca.eu.shop.>`;
   - **after a disconnect:** the consumer the servers keep for the leaf,
     `leaf-<accessor ID of the token>`, in the stream `NORCA_EVENTS_eu`,
     through the JetStream domain `norca-eu`:

     ```go
     js, _ := jetstream.NewWithDomain(nc, "norca-eu")
     cons, _ := js.Consumer(ctx, "NORCA_EVENTS_eu", "leaf-"+accessorID)
     ```

   A fetch waits at most 30 seconds; fetch in batches.

## What the events give today, and what they do not

- Every change of the namespace in the order of the Raft index, at least
  once, with the headers `Nomad-Index`, `Nomad-Topic`, `Nomad-Type`,
  `Norca-Job`.
- Job events without what may hold a secret (`Norca-Redacted: true`);
  service events without the outputs of the checks.
- Consul KV notices say which key changed, never the value.
- Who made a change is in the admin events and the audit of the secret
  store, which need the rights named in step 2.
- The stream keeps 72 hours by default: a trail that must last longer has
  to be stored by the reader.

## Taking it back

Deleting the token closes the leaf within seconds and the leader removes its
consumers. The NATS keeps running without the changes.
