module github.com/bacanoo-io/norca/rigger/cli

go 1.27

require (
	github.com/bacanoo-io/norca/norca v0.0.0
	github.com/bacanoo-io/norca/norca/api v0.0.0-20230103221135-ce00d683f9be
	github.com/bacanoo-io/norca/rigger v0.0.0
	github.com/hashicorp/go-cty-funcs v0.0.0-20200930094925-2721b1e36840
	github.com/hashicorp/hcl/v2 v2.20.2-0.20240517235513-55d9c02d147d
	github.com/mitchellh/reflectwalk v1.0.2
	github.com/zclconf/go-cty v1.13.0
	github.com/zclconf/go-cty-yaml v1.0.3
)

require (
	github.com/agext/levenshtein v1.2.1 // indirect
	github.com/apparentlymart/go-cidr v1.0.1 // indirect
	github.com/apparentlymart/go-textseg/v13 v13.0.0 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/bmatcuk/doublestar v1.1.5 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/hashicorp/cronexpr v1.1.2 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-rootcerts v1.0.2 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
)

// The library is in the same repository and is versioned with it.
replace github.com/bacanoo-io/norca/rigger => ../

// The norca program module is in the same repository (until step 4.1 of
// design/rigger-integration.md moves the decoders into norca/spec). The other
// replacements are the program module's own (norca/go.mod); a replacement
// in a dependency does not apply here, so they are repeated.
replace (
	github.com/Microsoft/go-winio => github.com/endocrimes/go-winio v0.4.13-0.20190628114223-fb47a8b41948
	github.com/armon/go-metrics => github.com/armon/go-metrics v0.0.0-20230509193637-d9ca9af9f1f9
	github.com/bacanoo-io/norca/norca => ../../norca
	github.com/bacanoo-io/norca/norca/api => ../../norca/api
	github.com/hashicorp/hcl => github.com/hashicorp/hcl v1.0.1-0.20201016140508-a07e7d50bbee
	github.com/hashicorp/nomad/api => ../../norca/compat/upstreamapi
	github.com/hashicorp/raft => github.com/bacanoo-io/raft v1.8.0-norca.1
)
