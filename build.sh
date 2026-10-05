#!/bin/sh
# Build the release binary: static, stripped, and free of this machine's paths.
#
#   CGO_ENABLED=0     static, and no C toolchain required. The only cgo-aware
#                     dependency is net, pulled in by the line editor for a
#                     feature this tool does not use, so disabling cgo costs
#                     nothing.
#   -ldflags="-s -w"  drop the symbol table and the DWARF debug info - this is
#                     where essentially all of the saving comes from: 4.7 MB
#                     plain -> 3.1 MB here, about a third
#   -trimpath         keep this machine's source paths out of the binary, so it
#                     does not leak where it was built (this does NOT shrink it
#                     in any meaningful way; it is hygiene for a binary you hand
#                     to other people)
#
# The one cost: a panic in a stripped binary still reports function names but no
# line numbers. Use a plain `go build` while debugging - the module is named
# `label` after the command, so a bare `go build` gives you `label` with no
# flags to remember.
set -eu
cd "$(dirname "$0")"
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath
ls -lh label
