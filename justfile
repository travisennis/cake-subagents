# List recipes.
default:
    @just --list

# Install this clone's git hooks: commit messages must be Conventional Commits.
hooks:
    git config core.hooksPath .githooks
    @echo "Installed .githooks (commit-msg)."

# Build the Go binary and the one public tool-name symlink in dist/.
build:
    go build -o dist/cake-subagent ./cmd/cake-subagent
    @for name in subagent_async subagent_cancel subagent_inbox subagent_peers subagent_send subagent_status subagent_wait; do \
        rm -f "dist/$name"; \
    done
    ln -sfn cake-subagent dist/subagent

# Run the offline integration suite against dist/. No model calls.
test: build
    CAKE_SUBAGENT_TOOLS="$PWD/dist" python3 -m unittest discover -s tests -v

# Format check, vet, and script syntax checks.
vet:
    go vet ./...
    @test -z "$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }
    sh -n bin/install
    sh -n bin/uninstall
    sh -n bin/doctor
    sh -n hooks/subagent-reports.sh
    sh -n .githooks/commit-msg

# Symlink the one tool into Cake's global toolbox directory and install the
# default SessionStart and Stop report hooks.
install: build
    ./bin/install

# Also install the opt-in PreToolUse report hook, which spawns a process on
# every tool call.
install-pretool: build
    ./bin/install --pre-tool-use

# Remove the links and report hook entries that point into this checkout.
uninstall: build
    ./bin/uninstall

# Verify that Cake will discover the one subagent tool.
doctor:
    ./bin/doctor
