#!/bin/sh
# Fails when the retyc-cli the controller embeds (Dockerfile: RETYC_CLI_IMAGE:RETYC_CLI_VERSION)
# differs from the mount pods' image (values.yaml: mount.image), as the chart renders it into
# mount.json. Needs helm and the chart's dependencies (helm dependency build). Runs from the root
# of the repository whatever the current directory: relative arguments are taken from there.
set -eu
cd "$(dirname "$0")/../.."

chart=${1:-charts/retyc-csi}
dockerfile=${2:-Dockerfile}

version=$(sed -n 's/^ARG RETYC_CLI_VERSION=//p' "$dockerfile" | head -n1 | tr -d '"')
repository=$(sed -n 's/^ARG RETYC_CLI_IMAGE=//p' "$dockerfile" | head -n1 | tr -d '"')
[ -n "$version" ] && [ -n "$repository" ] || {
  echo "::error file=$dockerfile::no default for ARG RETYC_CLI_VERSION or RETYC_CLI_IMAGE"
  exit 1
}

image=$(helm template check "$chart" --show-only templates/mount-config.yaml |
  sed -n 's/^ *"image": *"\(.*\)",\{0,1\}$/\1/p')
[ -n "$image" ] || { echo "::error::no image in the rendered mount.json"; exit 1; }

if [ "$image" != "$repository:$version" ]; then
  echo "::error file=$chart/values.yaml::mount.image is $image, the Dockerfile embeds $repository:$version: bump both together"
  exit 1
fi
echo "retyc-cli $repository:$version in both the Dockerfile and the chart"
