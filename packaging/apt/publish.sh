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
# Publish the .debs in bin/deb: as a GitHub release of the tag, and into
# the signed apt repository served by download.kalopa.com. Called by
# `make publish`:
#   publish.sh <tag> <signing-key>
# with KUBE_CONTEXT, KUBE_NS, KUBE_TARGET and DPKG_DIR in the environment:
# the repository lives in DPKG_DIR inside KUBE_TARGET (the nginx
# deployment), reached with kubectl exec and tar.
#
# The repository is pulled down first, so it keeps every earlier
# version; the new packages join its pool, the indexes are rebuilt, and
# Release is signed here, with the key on this machine.
# Needs gh, kubectl, gpg and apt-ftparchive (apt-utils).
#
set -eu

tag=$1 key=$2
: "${KUBE_CONTEXT:?}" "${KUBE_NS:?}" "${KUBE_TARGET:?}" "${DPKG_DIR:?}"
# kx runs a command in the deployment; kxi also feeds it stdin. Only
# push uses -i: with it, kubectl ends the session when its own stdin
# closes, which cuts short a tar being read back.
kx() { kubectl --context="$KUBE_CONTEXT" -n "$KUBE_NS" exec "$KUBE_TARGET" -- "$@"; }
kxi() { kubectl --context="$KUBE_CONTEXT" -n "$KUBE_NS" exec -i "$KUBE_TARGET" -- "$@"; }
# push <paths...>: copy them (relative to the local repository) into
# DPKG_DIR, as tar, owned by whoever the server runs as.
push() {
	tar -C "$repo" --owner=0 --group=0 --numeric-owner -cf - "$@" |
		kxi sh -c "mkdir -p '$DPKG_DIR' && tar -C '$DPKG_DIR' -o -xf -"
}
here=$(cd "$(dirname "$0")" && pwd)
top=$(cd "$here/../.." && pwd)
debs=$top/bin/deb
repo=$top/bin/dpkg
suite=stable
arches="arm64 armhf amd64"

for t in gh kubectl gpg apt-ftparchive; do
	command -v $t >/dev/null || { echo "publish: $t is needed" >&2; exit 1; }
done
ls "$debs"/*.deb >/dev/null 2>&1 || { echo "publish: no packages in $debs" >&2; exit 1; }
# Fail now, not halfway through, if the key can't sign (this is where
# gpg asks for the passphrase).
echo test | gpg --local-user "$key" --clearsign >/dev/null

# 1. The GitHub release.
(cd "$debs" && sha256sum *.deb > SHA256SUMS)
if gh release view "$tag" >/dev/null 2>&1; then
	gh release upload "$tag" "$debs"/*.deb "$debs/SHA256SUMS" --clobber
else
	gh release create "$tag" "$debs"/*.deb "$debs/SHA256SUMS" \
		--title "$tag" --generate-notes --verify-tag
fi

# 2. The apt repository: pull, add, index, sign, push.
rm -rf "$repo"
mkdir -p "$repo"
if kx test -d "$DPKG_DIR/pool"; then
	kx tar -C "$DPKG_DIR" -cf - pool | tar -C "$repo" -xf -
else
	echo "publish: no repository at $KUBE_TARGET:$DPKG_DIR yet; starting one"
fi
pool=$repo/pool/main/f/foudra-printman
mkdir -p "$pool"
cp "$debs"/*.deb "$pool/"

cd "$repo"
for a in $arches; do
	d=dists/$suite/main/binary-$a
	mkdir -p "$d"
	apt-ftparchive --arch "$a" packages pool > "$d/Packages"
	gzip -9kf "$d/Packages"
done
apt-ftparchive \
	-o APT::FTPArchive::Release::Origin="Kalopa Robotics" \
	-o APT::FTPArchive::Release::Label="Kalopa Robotics" \
	-o APT::FTPArchive::Release::Suite=$suite \
	-o APT::FTPArchive::Release::Codename=$suite \
	-o APT::FTPArchive::Release::Architectures="$arches" \
	-o APT::FTPArchive::Release::Components=main \
	release dists/$suite > Release.tmp
mv Release.tmp dists/$suite/Release
gpg --yes --local-user "$key" --clearsign -o dists/$suite/InRelease dists/$suite/Release
gpg --yes --local-user "$key" --armor --detach-sign -o dists/$suite/Release.gpg dists/$suite/Release
gpg --export "$key" > kalopa.gpg
install -m 0644 "$here/setup.sh" setup.sh

# Packages first, then the signed indexes that name them, so apt never
# sees an index whose files aren't there yet.
push pool kalopa.gpg setup.sh
push dists
echo "published $tag to GitHub and $KUBE_TARGET:$DPKG_DIR"
