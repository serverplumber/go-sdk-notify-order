# Justfile (bash mode)
# Usage: just check
set shell := ["bash", "-eo", "pipefail", "-c"]

# -----------------------------
# Config
# -----------------------------
go := env("GO", "go")

# List available recipes
default:
    @just --list

# Vet, then run the tests under the race detector
check:
    {{go}} vet ./...
    {{go}} test -race ./...

# Run the reproduction tests verbosely
repro:
    {{go}} test -race -count=1 -v -run 'ProgressBeforeResult|ProxyForwardsProgress' ./...
