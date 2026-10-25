module github.com/hashicorp/nomad/natsbridge

go 1.27

toolchain go1.27.3

// The bridge uses only the API module and the controller SDK of the fork;
// never the main Nomad module.
replace (
	github.com/hashicorp/nomad/api => ../api
	github.com/hashicorp/nomad/controller => ../controller
)

require (
	github.com/hashicorp/nomad/api v0.0.0-20230103221135-ce00d683f9be
	github.com/hashicorp/nomad/controller v0.0.0-00010101000000-000000000000
	github.com/nats-io/nats-server/v2 v2.15.0
	github.com/nats-io/nats.go v1.54.0
	github.com/shoenig/test v0.6.6
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.8.0-default-no-op // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/hashicorp/cronexpr v1.1.2 // indirect
	github.com/hashicorp/errwrap v1.0.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-rootcerts v1.0.2 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/nats-io/jwt/v2 v2.8.2 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20230728194245-b0cb94b80691 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/time v0.16.0 // indirect
)
