# List recipes.
default:
    @just --list

# Install this clone's git hooks: commit messages must be Conventional Commits.
hooks:
    git config core.hooksPath .githooks
    @echo "Installed .githooks (commit-msg)."

# Build the Go binary and the seven tool-name symlinks in dist/.
build:
    go build -o dist/cake-subagent ./cmd/cake-subagent
    @for name in subagent_async subagent_cancel subagent_inbox subagent_peers subagent_send subagent_status subagent_wait; do \
        ln -sfn cake-subagent "dist/$name"; \
    done

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
    sh -n hooks/subagent-inbox.sh
    sh -n .githooks/commit-msg

# Symlink the seven tools into Cake's global toolbox directory.
install: build
    ./bin/install

# Build, install, and link the opt-in mailbox push hook.
install-hook: build
    ./bin/install --hook

# Remove only links that point into this checkout.
uninstall:
    ./bin/uninstall

# Verify that Cake will discover all seven tools.
doctor:
    ./bin/doctor
