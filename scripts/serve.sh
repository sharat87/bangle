#!/bin/bash -eu
# Runs Bangle on 127.0.0.1:7060 in a window of the "daemons" tmux server,
# restarting it if it's already running. `make` calls this. The server's
# output goes to .logs/server.log.

SOCKET=daemons
WINDOW=bangle
PORT=7060
LOG_DIR="$PWD/.logs"
LOG_FILE="$LOG_DIR/server.log"

command -v tmux >/dev/null || { echo "serve: tmux is not installed" >&2; exit 1; }
test -f .env || { echo "serve: no .env here; cp .env.example .env and fill it in" >&2; exit 1; }

# Stop the running one first. Killing the window sends SIGHUP, which Bangle
# treats like SIGTERM: it closes upstream sessions, ending stdio servers'
# processes. Wait for the port to come free, then stop anything else still
# holding it (-sTCP:LISTEN: the server, not clients connected to it).
tmux -L "$SOCKET" kill-window -t "$WINDOW" 2>/dev/null || true
for _ in $(seq 50); do
	lsof -tiTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1 || break
	sleep 0.1
done
if pids=$(lsof -tiTCP:$PORT -sTCP:LISTEN 2>/dev/null); then
	echo "serve: stopping what still listens on $PORT: $pids"
	kill $pids
	sleep 1
	if lsof -tiTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1; then
		echo "serve: port $PORT is still in use" >&2
		exit 1
	fi
fi

mkdir -p "$LOG_DIR"
: > "$LOG_FILE"

if ! tmux -L "$SOCKET" has-session 2>/dev/null; then
	tmux -L "$SOCKET" new-session -d
fi

# -buildvcs=true stamps the git commit, which `go run` otherwise leaves out,
# so the UI's footer can show it.
tmux -L "$SOCKET" new-window -d -n "$WINDOW" -c "$PWD" "bash -lc '
set -aeu
source .env
exec go run -buildvcs=true ./cmd/server > \"$LOG_FILE\" 2>&1
'"

printf 'bangle: http://127.0.0.1:%s, log: %s\n' "$PORT" "$LOG_FILE"
