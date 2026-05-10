#!/usr/bin/env bash
exec go run -C "$(cd "$(dirname "$0")" && pwd)/tools/build" ./cmd/make "$@"
