#!/bin/sh
# Build the release binary: static, stripped, and free of this machine's paths.
#
#   CGO_ENABLED=0     static, and no C toolchain required
#   -ldflags="-s -w"  drop the symbol table and the DWARF debug info - this is
#                     where essentially all of the saving comes from
#   -trimpath         keep this machine's source paths out of the binary, so it
#                     does not leak where it was built (this does NOT shrink it
#                     in any meaningful way; it is hygiene for a binary you hand
#                     to other people)
#
# The one cost: a panic in a stripped binary still reports function names but no
# line numbers. Use a plain `go build` while debugging.
set -eu
cd "$(dirname "$0")"
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath
ls -lh label
