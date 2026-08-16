#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/../.."
[ -f test/realworld/env ] && source test/realworld/env
CGO_ENABLED=0 go test -tags realworld -run 'TestReal' -v ./adapters/... "$@"
