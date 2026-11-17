// Not built on its own: the files are added to norca's jobspec2 package for
// one test run with go test -overlay (run.sh). This go.mod keeps them out of
// the modules of Rig.
module spike/jobspec2decode

go 1.27
