# Native mTLS: sidecar, upstreams and the cross-region gateway

Norca issues every workload of a task group with an `mtls` service a short-lived SPIFFE certificate
(`spiffe://<trust-domain>/ns/<namespace>/job/<job>/group/<group>`, plus the DNS names
`<service>.service.<namespace>[.<region>].nomad` of its mtls services) and decides with `ServiceIntention`
objects who may connect to whom. Applications can use the certificate themselves (`mode = "app"`, see the
`api/mtls` Go library), or let an Envoy sidecar do it (`mode = "sidecar"`). This page covers the sidecar mode,
upstreams and the gateway between regions. No Consul is involved.

Design and implementation notes: [consul-independence-plan.md, item 5](design/consul-independence-plan.md#5-native-mtls-and-service-authorization).

## Sidecar mode

```hcl
job "web" {
  group "web" {
    network {
      mode = "bridge"
    }

    service {
      name     = "web"
      port     = "8080"          # the port the application listens on inside the group network namespace
      provider = "nomad"

      mtls {
        mode = "sidecar"

        upstream "api" {
          service    = "api"     # in the namespace of the job, in this region
          local_port = 9001      # the application connects to 127.0.0.1:9001
        }

        upstream "db" {
          service    = "postgres"
          namespace  = "data"
          local_port = 5432
        }

        upstream "eu-cache" {
          service    = "redis"
          region     = "eu"      # through the gateway of region eu
          local_port = 6379
        }
      }
    }

    task "web" {
      driver = "docker"
      config {
        image = "example/web:1"
      }
    }
  }
}
```

When the job is registered, the servers add to the group:

- a prestart sidecar task `mtls-proxy-<service>` (kind `mtls-proxy:<service>`, driver docker, or podman if the
  group only uses podman) running the official Envoy image, 100 MHz / 128 MB;
- a dynamic port `mtls-proxy-<service>` for the inbound listener of the sidecar.

The service is registered in the catalog with the address and port of the sidecar, so everybody reaching
it through the catalog, the built-in DNS or the Consul-compatible layer connects with mTLS. Its registrations
carry the meta `norca-mtls = "sidecar"` (`"app"` for app mode services, `"gateway"` for gateways).

The sidecar:

- **inbound**: terminates mTLS on the injected port, requires a client certificate signed by a CA of the
  federated trust bundle (verified per SPIFFE trust domain), authorizes the peer with the service intentions
  of the namespace and forwards the plain TCP stream to `127.0.0.1:<port>` of the application;
- **upstreams**: listens on `127.0.0.1:<local_port>` in the network namespace of the group and opens an mTLS
  connection to a healthy instance of the upstream service, with its workload certificate; it verifies that
  the server certificate carries `<service>.service.<namespace>.<region>.nomad` and is signed by a CA of that
  region (not by the CA of another region of the federation), and sends that name as SNI.

The application port should not be mapped to the host: use a port number (`port = "8080"`) or a group port
label without a host mapping, otherwise the application is also reachable without mTLS. Nomad service checks
still use the application port.

Changing `mtls` blocks of sidecar or gateway services is a destructive update. Intentions, upstream instances,
certificate renewals and CA rotations are applied to the running Envoy without restarting it.

### Authorization

Intentions are evaluated like the `api/mtls` library: the most specific matching intention decides (exact
destination, then exact source namespace, then exact source service), deny wins at equal specificity, and
without a matching intention the connection is denied. The source namespace comes from the SPIFFE ID of the
peer certificate, the source services from its DNS names.

```shell
norca service intention create web api
norca service intention create -deny 'batch/*' api
```

A change applies to new connections within about a second. The intentions are part of the inbound listener,
so a change to the intentions of the service replaces it: connections that were established before keep
running on the previous listener version until Envoy has drained it (`--drain-time-s`, 600 s by default), then
they are closed and the clients reconnect under the new rules. Changes to intentions of other services, new
certificates and changed upstream instances don't touch the listener. Restart the sidecar task to cut the
existing connections immediately.

### Envoy image

The image is set by node meta, so air-gapped clusters can use a mirror and pin a digest:

| Node meta | Default |
| --- | --- |
| `norca.mtls.envoy_image` | unset: built from the four keys below |
| `norca.mtls.envoy_registry` | `docker.io` |
| `norca.mtls.envoy_repository` | `envoyproxy/envoy` |
| `norca.mtls.envoy_tag` | `v1.40.1` |
| `norca.mtls.envoy_digest` | unset (`sha256:...` to pin) |
| `norca.mtls.envoy_log_level` | `info` |

```hcl
client {
  meta {
    "norca.mtls.envoy_registry" = "registry.internal:5000"
    "norca.mtls.envoy_digest"   = "sha256:..."
  }
}
```

Norca does not distribute Envoy; the client pulls the image like any other task image.

To change the resources, the driver or anything else of the proxy task, define the task in the group yourself
with `kind = "mtls-proxy:<service>"` (or `"mtls-gateway:<service>"`); the servers then don't inject one. Its
configuration is in `${NOMAD_SECRETS_DIR}/envoy/bootstrap.json`.

## Cross-region gateway

Each region that offers services to other regions runs a gateway: a group with a service in `gateway` mode.

```hcl
job "mtls-gateway" {
  type = "system"          # or a service job with a few instances

  group "gateway" {
    network {
      mode = "bridge"
      port "mtls" {
        static = 8443
        to     = 8443
      }
    }

    service {
      name     = "mtls-gateway"   # the name upstreams look for by default
      port     = "mtls"
      provider = "nomad"

      mtls {
        mode = "gateway"
      }
    }
  }
}
```

Run it in the `default` namespace. The servers inject the Envoy task `mtls-gateway-<service>` (host networking
is supported as well). The gateway listens on the service port, reads the SNI name of each TLS connection and
forwards the connection, without terminating it, to a healthy instance of the mtls service
`<service>.service.<namespace>.<region>.nomad` of its region (registrations with `norca-mtls` = `app` or
`sidecar`, of every namespace). Connections with any other name are closed. mTLS stays end to end between the
two sidecars; the intentions of the destination region decide.

An upstream with `region = "<other region>"` connects to the instances of the gateway service of that region
(`gateway = "<name>"` selects another gateway service of its `default` namespace). The servers of the two
regions must be federated, the regions must share the trust domain or trust each other's roots
(`server { mtls { federation = true } }`), and the nodes must reach the gateway port of the other region
(for example over the VPN between the clouds).

With ACLs enabled, the client reads the catalog of another region with the token of its `dns` block
(`client { dns { token = "..." } }`), which must be valid in every region (a global token with `read-job` on
the namespaces of the gateways); without it, the node identity is used, which other regions don't accept.

## Limits

- TCP only: no HTTP routing, retries or L7 intentions.
- One sidecar per sidecar mode service; all of them share the network namespace of the group, so the
  `local_port` values of a group must differ.
- Sidecar and gateway modes are only supported on group services with exactly one network.
- The gateway forwards to every mtls service of its region; intentions protect the services.
- Envoy's admin interface is disabled.
