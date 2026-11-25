#!/bin/sh
# Cake hook entry for subagent reports.
#
# bin/install links this script into ${XDG_CONFIG_HOME:-$HOME/.config}/cake/hooks
# and adds SessionStart and Stop hook groups that run it. Resolve this script
# through that symlink so the checkout can move without reinstalling.
#
# The hook payload arrives on stdin; the decision goes to stdout. Diagnostics
# stay on stderr, and a failure is fail-open because Cake logs it and continues.
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
exec "$root/dist/cake-subagent" event
