#!/bin/sh
#
# Copyright (c) 2026, Kalopa Robotics Limited.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Build one .deb from an already-built binary. Called by `make deb`:
#   build.sh <debian-arch> <version> <maintainer> <binary> <out-dir>
# Needs only dpkg-deb, which every Debian and Ubuntu machine has.
#
set -eu

arch=$1 version=$2 maintainer=$3 binary=$4 out=$5
here=$(cd "$(dirname "$0")" && pwd)
top=$(cd "$here/../.." && pwd)
pkg=foudra-printman
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
chmod 0755 "$root" # mktemp makes it 0700, which would become the mode of /

install -D -m 0755 "$binary" "$root/usr/bin/print-manager"
install -D -m 0600 "$here/env" "$root/etc/print-manager/env"
install -D -m 0644 "$here/copyright" "$root/usr/share/doc/$pkg/copyright"
install -D -m 0644 "$top/README.md" "$root/usr/share/doc/$pkg/README.md"

# The same unit as a hand install, but from /usr/bin, and not started
# until the env file has an API key: ExecCondition skips the start
# (rather than failing it every five seconds) on a fresh install. The
# + runs the check as root, since the env file is root-only.
mkdir -p "$root/lib/systemd/system"
sed -e 's|/usr/local/bin/print-manager|/usr/bin/print-manager|' \
    -e '/^ExecStart=/i\
ExecCondition=+/bin/sh -c "grep -q ^MRP_API_KEY=. /etc/print-manager/env"' \
    "$top/deploy/print-manager.service" > "$root/lib/systemd/system/print-manager.service"
chmod 0644 "$root/lib/systemd/system/print-manager.service"

mkdir -p "$root/DEBIAN"
size=$(du -sk "$root" | cut -f1)
sed -e "s|@VERSION@|$version|" -e "s|@ARCH@|$arch|" \
    -e "s|@MAINTAINER@|$maintainer|" -e "s|@SIZE@|$size|" \
    "$here/control.in" > "$root/DEBIAN/control"
echo /etc/print-manager/env > "$root/DEBIAN/conffiles"
for s in postinst prerm postrm; do
	install -m 0755 "$here/$s" "$root/DEBIAN/$s"
done

mkdir -p "$out"
dpkg-deb --root-owner-group -Zxz --build "$root" "$out/${pkg}_${version}_${arch}.deb" >/dev/null
echo "$out/${pkg}_${version}_${arch}.deb"
