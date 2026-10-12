#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
claude plugin test plugins/tether-mail/claude
