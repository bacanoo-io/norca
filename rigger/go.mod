module github.com/bacanoo-io/norca-rig

go 1.27

require (
	github.com/agext/levenshtein v1.2.1
	// The versions of hcl and go-cty are those of Norca (norca/norca/go.mod), so
	// that the server keeps its own when it embeds the library.
	github.com/hashicorp/hcl/v2 v2.20.2-0.20240517235513-55d9c02d147d
	github.com/zclconf/go-cty v1.13.0
	golang.org/x/crypto v0.57.0
)

require (
	github.com/apparentlymart/go-textseg/v13 v13.0.0 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/mitchellh/go-wordwrap v0.0.0-20150314170334-ad45545899c7 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
)
