#!/bin/sh
# Opt-in push delivery for the async subagent mailbox.
# No-op unless this Cake process is an async subagent run.
# Resolve this script through the symlink bin/install creates.
set -eu

self=$0
while [ -L "$self" ]; do
    target=$(readlink "$self")
    case "$target" in
        /*) self=$target ;;
        *) self=$(dirname "$self")/$target ;;
    esac
done
root=$(cd "$(dirname "$self")/.." && pwd -P)
exec /usr/bin/ruby "$root/bin/cake-subagent" drain
