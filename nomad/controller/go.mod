module github.com/hashicorp/nomad/controller

go 1.27

toolchain go1.27.3

// The SDK uses only the API module of the fork; never the main Nomad module.
replace github.com/hashicorp/nomad/api => ../api

require (
	github.com/hashicorp/nomad/api v0.0.0-20230103221135-ce00d683f9be
	github.com/shoenig/test v0.6.6
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/hashicorp/cronexpr v1.1.2 // indirect
	github.com/hashicorp/errwrap v1.0.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-rootcerts v1.0.2 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20230728194245-b0cb94b80691 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
