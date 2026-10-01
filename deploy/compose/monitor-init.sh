#!/bin/sh
set -eu
# Prometheus runs as nobody; the isolated volume is mounted only by these two services.
printf '%s' "${CONTROL_PLANE_TOKEN:-}" > /run/monitor/control-plane.token
chmod 644 /run/monitor/control-plane.token
