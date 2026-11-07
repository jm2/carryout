#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Builds a .deb from a prebuilt binary with dpkg-deb (no debhelper).
# carryout is a single static binary with no services or config, so the
# package has no maintainer scripts.
#
#   build-aux/linux/build-deb.sh <version> <amd64|arm64> <dir with carryout> <output .deb>
#
# DEB_MAINTAINER sets the Maintainer field.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 <version> <amd64|arm64> <bindir> <output.deb>" >&2
  exit 2
fi
version=${1#v}
arch=$2
bindir=$3
out=$4
root=$(cd "$(dirname "$0")/../.." && pwd)
maintainer=${DEB_MAINTAINER:-carryout contributors <carryout@example.invalid>}

case $arch in
  amd64 | arm64) ;;
  *) echo "unsupported architecture $arch" >&2; exit 2 ;;
esac
# Debian sorts "~" before anything, so 1.0.0~rc.1 precedes 1.0.0.
deb_version=${version//-/\~}
if [[ ! $deb_version =~ ^[0-9][A-Za-z0-9.+~]*$ ]]; then
  echo "version $version is not a valid Debian version" >&2
  exit 2
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
chmod 0755 "$stage"
"$root/build-aux/linux/install-tree.sh" "$stage" "$bindir" /usr

doc="$stage/usr/share/doc/carryout"
cat > "$doc/copyright" <<'EOF'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: carryout
Source: https://github.com/jm2/carryout

Files: *
Copyright: 2026 John-Michael and carryout contributors
License: GPL-3.0-or-later
 On Debian systems, the full text of the GNU General Public License
 version 3 can be found in /usr/share/common-licenses/GPL-3.
EOF
chmod 0644 "$doc/copyright"
{
  printf 'carryout (%s) unstable; urgency=medium\n\n' "$deb_version"
  printf '  * Release %s. See https://github.com/jm2/carryout/releases\n\n' "$version"
  printf ' -- %s  %s\n' "$maintainer" "$(date -R -u -d "@${SOURCE_DATE_EPOCH:-$(date +%s)}")"
} | gzip -9n > "$doc/changelog.gz"
chmod 0644 "$doc/changelog.gz"
# Go binaries are statically linked by design (CGO_ENABLED=0). The manual
# is `carryout -h` and the README.
install -Dm644 /dev/stdin "$stage/usr/share/lintian/overrides/carryout" <<'EOF'
carryout: statically-linked-binary [usr/bin/carryout]
carryout: no-manual-page [usr/bin/carryout]
EOF

mkdir -p "$stage/DEBIAN"
installed_size=$(du -sk --exclude=DEBIAN "$stage" | cut -f1)
cat > "$stage/DEBIAN/control" <<EOF
Package: carryout
Version: $deb_version
Architecture: $arch
Maintainer: $maintainer
Installed-Size: $installed_size
Section: web
Priority: optional
Homepage: https://github.com/jm2/carryout
Description: download helper for Google Takeout
 carryout fetches every archive part of a Google Takeout export, given one
 download request copied from the browser's developer tools and the file
 list from the Takeout page. It verifies each archive as it streams, pauses
 for fresh cookies when the session dies instead of wasting download
 attempts, and resumes interrupted parts.
EOF
(cd "$stage" && find usr -type f -print0 | LC_ALL=C sort -z | xargs -0 md5sum) > "$stage/DEBIAN/md5sums"
chmod 0644 "$stage/DEBIAN/control" "$stage/DEBIAN/md5sums"

mkdir -p "$(dirname "$out")"
dpkg-deb --root-owner-group -Zxz --build "$stage" "$out"
dpkg-deb --info "$out"
