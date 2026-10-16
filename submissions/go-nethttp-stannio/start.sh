#!/usr/bin/env bash
# Runs the server in the foreground. Config: DATABASE_URL, JWT_SECRET, HOST, PORT.
# GC only near the memory limit: the live heap is small (in-flight requests + idle connections), so the
# default "GC every time the heap doubles" would just burn CPU. 3 GiB leaves plenty for Postgres on 16 GB.
set -euo pipefail
export DB_POOL_SIZE="${DB_POOL_SIZE:-$(( 10 * $(nproc 2>/dev/null || sysctl -n hw.ncpu) ))}"
export GOGC="${GOGC:-off}" GOMEMLIMIT="${GOMEMLIMIT:-3GiB}"
exec "$(cd "$(dirname "$0")" && pwd)/bin/conduit"
