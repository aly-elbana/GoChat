#!/usr/bin/env bash
# Opens a 4-pane tmux layout for recording the demo video:
#
#   ┌──────────────┬──────────────┐
#   │   server     │    alice     │
#   ├──────────────┼──────────────┤
#   │     bob      │    carol     │
#   └──────────────┴──────────────┘
#
# Usage: ./scripts/demo.sh [port]
# Requires tmux. On Windows/macOS without tmux, just open four terminals and
# run the commands printed in the README.
set -euo pipefail

PORT="${1:-9090}"
ADDR="127.0.0.1:${PORT}"
SESSION="gochat-demo"
cd "$(dirname "$0")/.."

command -v tmux >/dev/null || { echo "tmux is required for this script"; exit 1; }

go build -o bin/gochat-server ./cmd/server
go build -o bin/gochat-client ./cmd/client

tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session  -d -s "$SESSION" -x "$(tput cols)" -y "$(tput lines)" "./bin/gochat-server -addr ${ADDR}; read"
tmux split-window -h -t "$SESSION":0.0 "sleep 1; ./bin/gochat-client -server ${ADDR} -name alice; read"
tmux split-window -v -t "$SESSION":0.0 "sleep 1.5; ./bin/gochat-client -server ${ADDR} -name bob; read"
tmux split-window -v -t "$SESSION":0.1 "sleep 2; ./bin/gochat-client -server ${ADDR}; read"
tmux select-layout -t "$SESSION" tiled
tmux set -t "$SESSION" mouse on
tmux select-pane -t "$SESSION":0.1
tmux attach -t "$SESSION"
