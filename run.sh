#!/bin/bash
set -a
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$DIR/../.env" ] && source "$DIR/../.env"
set +a

# Ensure homebrew binaries (like mosh-server) and standard paths are in PATH
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
export PORT=${PORT:-8090}

cd "$DIR"
exec "$DIR/mosh-webrtc-server"
