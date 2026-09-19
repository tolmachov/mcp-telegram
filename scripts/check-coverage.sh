#!/bin/sh
set -eu

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

coverage() {
	pattern=$1
	name=$2
	profile="$tmp_dir/$name.cover"
	go test -coverprofile="$profile" "$pattern" >/dev/null
	go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}'
}

check() {
	name=$1
	actual=$2
	required=$3
	if ! awk -v actual="$actual" -v required="$required" 'BEGIN {exit !(actual + 0 >= required + 0)}'; then
		echo "coverage gate failed: $name is $actual%, required >= $required%" >&2
		exit 1
	fi
	echo "coverage gate passed: $name is $actual% (required >= $required%)"
}

check total "$(coverage ./... total)" 70
check tools "$(coverage ./internal/tools tools)" 65
check messages "$(coverage ./internal/messages messages)" 80
check server "$(coverage ./internal/server server)" 80
check authsrv "$(coverage ./internal/authsrv authsrv)" 90
