#!/bin/sh
set -eu

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

check_coverage() {
	pattern=$1
	name=$2
	required=$3
	profile="$tmp_dir/$name.cover"
	# Keep failing test output and status. A failed test can still produce a
	# complete profile, so coverage must never substitute for test success.
	if go test -coverprofile="$profile" "$pattern" >"$tmp_dir/$name.log" 2>&1; then
		:
	else
		cat "$tmp_dir/$name.log" >&2
		return 1
	fi
	go tool cover -func="$profile" >"$tmp_dir/$name.report" || return 1
	actual=$(awk '/^total:/ {gsub(/%/, "", $3); print $3}' "$tmp_dir/$name.report")
	if ! awk -v actual="$actual" -v required="$required" 'BEGIN {exit !(actual + 0 >= required + 0)}'; then
		echo "coverage gate failed: $name is $actual%, required >= $required%" >&2
		exit 1
	fi
	echo "coverage gate passed: $name is $actual% (required >= $required%)"
}

check_coverage ./... total 70
check_coverage ./internal/tools tools 65
check_coverage ./internal/messages messages 80
check_coverage ./internal/server server 80
check_coverage ./internal/authsrv authsrv 90
