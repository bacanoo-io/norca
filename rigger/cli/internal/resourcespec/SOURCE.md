# A temporary copy of a file of norca's resourcespec

`object.go` of `norca/helper/resourcespec` of norca, commit
`0c691dfc0100e27f0ea4f5f13f54a7bef8183501`, unchanged, with its own header (Norca contributors,
MPL-2.0): the parsers of object kinds and objects, the ones the servers use
for the resource files of a pack.

**Why a copy:** Rig gives the host an object kind as its block written as
HCL in the order of the module (host.ResourceDecoder), and `cli/norcaapi`
decodes it with the parser of Norca, so that a kind of a module is the kind
the same file makes with `norca object-kind apply` and with the pack
manager. The rest of the package needs packages of norca's server module,
which the rig program does not link; norca is not changed before phase 7.
**When it goes:** in phase 7, with the copy of jobspec2 (`cli/norcaapi`
then imports norca's package).

The copy is not edited, linted or formatted here (`.golangci.yml`).
`make check-copy` compares it with the norca checkout next to this
repository and fails when they differ: then copy the file again and update
the commit above.
