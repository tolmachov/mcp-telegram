#!/bin/sh
# Exercise the real Go test/coverage process, including a failed test that
# still produces 100% coverage. Run on Linux as well as macOS (/bin/sh differs).
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
cd "$tmp_dir"
printf 'module example.com/coverage-test\n\ngo 1.26\n' > go.mod
for package in tools messages server authsrv; do
 mkdir -p "internal/$package"
 cat > "internal/$package/sample.go" <<'GO'
package sample
func covered() bool { return true }
GO
 cat > "internal/$package/sample_test.go" <<'GO'
package sample
import ("os"; "testing")
func TestCoverageFixture(t *testing.T) {
 if os.Getenv("COVERAGE_TEST_MODE") == "low" { return }
 if covered() && os.Getenv("COVERAGE_TEST_MODE") == "fail" {
  t.Fatal("intentional coverage regression fixture failure")
 }
}
GO
done

COVERAGE_TEST_MODE=pass "$script_dir/check-coverage.sh" > pass.log 2>&1 || { cat pass.log; exit 1; }
for mode in fail low; do
 if COVERAGE_TEST_MODE=$mode "$script_dir/check-coverage.sh" > "$mode.log" 2>&1; then
  cat "$mode.log"
  echo "coverage script incorrectly accepted $mode fixture" >&2
  exit 1
 fi
done
# The failing assertion must be visible, rather than discarded by redirection.
if ! grep -q 'intentional coverage regression fixture failure' fail.log; then
 cat fail.log
 echo 'coverage script hid the failing test output' >&2
 exit 1
fi
if ! grep -q 'coverage gate failed' low.log; then
 cat low.log
 echo 'low coverage failed for an unexpected reason' >&2
 exit 1
fi
echo 'coverage script regression tests passed'
