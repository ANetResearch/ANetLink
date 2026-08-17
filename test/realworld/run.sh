#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/../.."
[ -f test/realworld/env ] && source test/realworld/env
# internal/... is included because the RTSP probe lives there and has its own
# L1 tier against a real media server.
CGO_ENABLED=0 go test -tags realworld -run 'TestReal' -v ./adapters/... ./internal/... "$@"
