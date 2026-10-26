# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# A single MinIO server as the S3 store of the scheduled backups, for this
# example only: in production keep the backups outside the cluster they
# protect. MinIO is AGPL-3.0 software by MinIO, Inc.; Norca only references
# the image, the node pulls it. Any S3-compatible store works the same way.
#
#   norca secret put minio/root user=norca-backup password=-
#   norca job run minio.nomad.hcl
job "minio" {
  type        = "service"
  datacenters = ["*"]

  group "minio" {
    count = 1

    # Static ports on the node: the servers reach the API at
    # http://<node address>:9000 (endpoint of the backup destination).
    network {
      port "s3" {
        static = 9000
        to     = 9000
      }
      port "console" {
        static = 9001
        to     = 9001
      }
    }

    volume "data" {
      type   = "host"
      source = "minio" # client { host_volume "minio" { ... } }
    }

    # Disruption budget of a single instance: evictions
    # (norca alloc stop -respect-disruption) are refused, and a node drain
    # moves it only when its deadline is reached.
    disruption {
      min_healthy = "1"
    }

    # The root credentials come from the built-in secret store.
    secret_access {
      path = "minio/*"
    }

    service {
      name     = "minio"
      provider = "nomad"
      port     = "s3"

      check {
        name            = "startup"
        role            = "startup"
        type            = "http"
        path            = "/minio/health/live"
        interval        = "5s"
        timeout         = "2s"
        startup_timeout = "2m"
      }

      check {
        name     = "readiness"
        role     = "readiness"
        type     = "http"
        path     = "/minio/health/ready"
        interval = "10s"
        timeout  = "2s"
      }

      check {
        name     = "liveness"
        role     = "liveness"
        type     = "http"
        path     = "/minio/health/live"
        interval = "15s"
        timeout  = "3s"
      }
    }

    task "minio" {
      driver = "docker"

      config {
        # TODO: verify the tag; pin a release (and its digest) you have
        # validated. MinIO publishes images under minio/minio and
        # quay.io/minio/minio.
        image = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
        args  = ["server", "/data", "--console-address", ":9001"]
        ports = ["s3", "console"]
      }

      volume_mount {
        volume      = "data"
        destination = "/data"
      }

      secret "root" {
        path = "minio/root"

        env {
          MINIO_ROOT_USER     = "user"
          MINIO_ROOT_PASSWORD = "password"
        }
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
