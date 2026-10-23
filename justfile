# List recipes.
default:
    @just --list

# Run the offline integration suite. No model calls.
test:
    python3 -m unittest discover -s tests -v

# Syntax-check every script.
lint:
    /usr/bin/ruby -c lib/subagent_async.rb
    /usr/bin/ruby -c bin/cake-subagent
    sh -n bin/install
    sh -n bin/uninstall
    sh -n bin/doctor
    sh -n hooks/subagent-inbox.sh

# Symlink the seven tools into Cake's global toolbox directory.
install:
    ./bin/install

# Install and link the opt-in mailbox push hook.
install-hook:
    ./bin/install --hook

# Remove only links that point into this checkout.
uninstall:
    ./bin/uninstall

# Verify that Cake will discover all seven tools.
doctor:
    ./bin/doctor
