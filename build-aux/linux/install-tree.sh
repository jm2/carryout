#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Installs carryout's files into a package staging tree. Every package
# (rpm, deb, Arch) uses this, so they all ship the same files:
#
#   <prefix>/bin/carryout
#   <prefix>/share/doc/carryout/README.md
#
# carryout is a plain CLI: no services, config files or accounts. Each
# package adds the license where its distribution keeps it.
#
#   build-aux/linux/install-tree.sh <destdir> <dir with carryout> [prefix]
set -eu

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <destdir> <bindir> [prefix]" >&2
  exit 2
fi
dest=$1
bindir=$2
prefix=${3:-/usr}
root=$(cd "$(dirname "$0")/../.." && pwd)

install -Dm755 "$bindir/carryout" "$dest$prefix/bin/carryout"
install -Dm644 "$root/README.md" "$dest$prefix/share/doc/carryout/README.md"
