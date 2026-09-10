#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../../.." && pwd)
enclave=${ENCLAVE:-rebroadcast-e2e-$(date +%Y%m%d%H%M%S)}
image=${BOR_IMAGE:-bor:rebroadcast-e2e}
artifacts=${ARTIFACTS:-$repo_dir/build/rebroadcast-e2e/$enclave}
package=$(mktemp -d "${TMPDIR:-/tmp}/bor-rebroadcast-package.XXXXXX")
mkdir -p "$artifacts"

if [[ -z ${BOR_IMAGE:-} ]]; then
  docker build -t "$image" --file "$repo_dir/Dockerfile" "$repo_dir"
fi

git -c advice.detachedHead=false clone --quiet --depth 1 --branch v1.4.2 https://github.com/0xPolygon/kurtosis-pos.git "$package"
python3 - "$package" "$script_dir/params-e2e.yml" "$image" <<'PY'
import pathlib
import sys

package = pathlib.Path(sys.argv[1])
config = package / "static_files/el/bor/config.toml"
text = config.read_text()
replacements = {
    'cache = 4096': 'cache = 256',
    'rebroadcast-interval = "10s"': 'rebroadcast-interval = "2s"',
    'rebroadcast-max-age = "1m"': 'rebroadcast-max-age = "30m"',
}
for old, new in replacements.items():
    if text.count(old) != 1:
        raise SystemExit(f"Expected one fixture setting: {old}")
    text = text.replace(old, new)
config.write_text(text)
params = pathlib.Path(sys.argv[2]).read_text()
import json
(package / "rebroadcast.yml").write_text(params.replace("bor:rebroadcast-e2e", json.dumps(sys.argv[3])))
PY

# Only the newly created test enclave is stopped; retain it and the package for diagnostics.
kurtosis enclave add --name "$enclave"
cleanup() {
  if [[ ${KEEP_ENCLAVE:-false} != true ]]; then
    kurtosis enclave stop "$enclave"
  fi
}
trap cleanup EXIT
echo "Starting test enclave $enclave (launch log: $artifacts/launch.log)"
if ! (
  cd -- "$package"
  kurtosis run --enclave "$enclave" --args-file rebroadcast.yml .
) >"$artifacts/launch.log" 2>&1; then
  tail -n 40 "$artifacts/launch.log"
  exit 1
fi
python3 "$script_dir/e2e.py" --enclave "$enclave" --artifacts "$artifacts" "$@"
