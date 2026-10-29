#!/bin/sh
# Build the image for the server's CPU, copy it over SSH and restart the stack.
#
#   deploy/deploy.sh <ssh-host> [linux/amd64|linux/arm64]
#
# The server needs Docker with the compose plugin and ~/ask-data-egov/.env
# (see deploy/README.md). Nothing is pushed to a registry: the image travels
# over the SSH connection.
set -eu

host=${1:?usage: deploy/deploy.sh <ssh-host> [platform]}
platform=${2:-linux/amd64}
dir=ask-data-egov

cd "$(dirname "$0")/.."

echo "==> building ask-data-egov:latest for $platform"
docker buildx build --platform "$platform" -t ask-data-egov:latest --load .

echo "==> copying the image to $host"
docker save ask-data-egov:latest | gzip -1 | ssh "$host" 'gunzip | docker load'

echo "==> updating the compose file and restarting"
ssh "$host" "mkdir -p ~/$dir"
scp -q deploy/docker-compose.yml "$host:$dir/docker-compose.yml"
ssh "$host" "cd ~/$dir && test -f .env || { echo 'missing ~/$dir/.env, see deploy/README.md' >&2; exit 1; }
  docker compose up -d --remove-orphans && docker image prune -f >/dev/null && docker compose ps"
