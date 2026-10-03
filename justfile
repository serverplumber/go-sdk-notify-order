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

# Vet, then run the tests
check:
    {{go}} vet ./...
    {{go}} test ./...

# Run the reproduction tests verbosely
repro:
    {{go}} test -count=1 -v -run 'ProgressBeforeResult|Proxy' ./...
