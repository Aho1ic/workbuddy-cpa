#!/usr/bin/env bash
# run_cpa.sh - 启动搭载 workbuddy-CPA 插件的 CLIProxyAPI 服务
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${SCRIPT_DIR}"

PLUGIN_FILE="${SCRIPT_DIR}/workbuddy.dylib"
if [[ "$(uname -s)" == "Linux" ]]; then
    PLUGIN_FILE="${SCRIPT_DIR}/workbuddy.so"
fi

if [[ ! -f "${PLUGIN_FILE}" ]]; then
    echo "==> Plugin binary not found. Running make build..."
    make build
fi

if [[ ! -f "${SCRIPT_DIR}/workbuddy.json" ]]; then
    echo "==> workbuddy.json not found. Attempting desktop import..."
    python3 scripts/import_desktop_auth.py -o "${SCRIPT_DIR}/workbuddy.json" || true
fi

echo "==> workbuddy-CPA plugin ready at: ${PLUGIN_FILE}"
if command -v cliproxyapi &>/dev/null; then
    exec cliproxyapi --config "${SCRIPT_DIR}/config/cpa_config.yaml.example"
elif command -v cpa &>/dev/null; then
    exec cpa --config "${SCRIPT_DIR}/config/cpa_config.yaml.example"
else
    echo "CLIProxyAPI binary ('cliproxyapi' or 'cpa') not found in PATH."
    echo "Please download CLIProxyAPI and launch with: cliproxyapi --config config/cpa_config.yaml.example"
    exit 0
fi
