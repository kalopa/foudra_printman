#!/bin/sh
#
# Add the Kalopa Robotics apt repository, then install the print manager:
#
#   curl -fsSL https://download.kalopa.com/dpkg/setup.sh | sudo sh
#
# It does only what the README's manual steps do: fetch the signing key
# into /etc/apt/keyrings, add one sources file, and run apt.
#
set -eu

url=https://download.kalopa.com/dpkg
[ "$(id -u)" = 0 ] || { echo "run as root (sudo sh)" >&2; exit 1; }
command -v curl >/dev/null || { apt-get update && apt-get install -y curl; }

install -d -m 0755 /etc/apt/keyrings
curl -fsSL "$url/kalopa.gpg" -o /etc/apt/keyrings/kalopa.gpg
chmod 0644 /etc/apt/keyrings/kalopa.gpg
echo "deb [signed-by=/etc/apt/keyrings/kalopa.gpg] $url stable main" > /etc/apt/sources.list.d/kalopa.list

apt-get update
apt-get install -y foudra-printman
