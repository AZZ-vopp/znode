#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
installer="$root/script/install.sh"
manager="$root/script/znode.sh"
readme="$root/README.md"
release_workflow="$root/.github/workflows/publish-release-assets.yml"

fail() {
    echo "security_scripts_test: $*" >&2
    exit 1
}

contains() {
    local file="$1"
    local text="$2"
    grep -Fq -- "$text" "$file" || fail "expected $(basename "$file") to contain: $text"
}

not_contains() {
    local file="$1"
    local text="$2"
    if grep -Fq -- "$text" "$file"; then
        fail "expected $(basename "$file") not to contain: $text"
    fi
}

extract_function_before() {
    local file="$1"
    local function_name="$2"
    local next_function="$3"
    sed -n "/^${function_name}() {$/,/^${next_function}() {$/p" "$file" | sed '$d'
}

bash -n "$installer"
bash -n "$manager"

for privileged_script in "$installer" "$manager"; do
    contains "$privileged_script" 'PATH=/usr/sbin:/usr/bin:/sbin:/bin'
    contains "$privileged_script" 'unset BASH_ENV ENV CDPATH GLOBIGNORE LD_PRELOAD LD_LIBRARY_PATH OPENSSL_CONF'
    contains "$privileged_script" 'umask 077'
    if grep -Eq '^PATH=.*(/usr/local/bin|/usr/local/sbin)' "$privileged_script"; then
        fail "privileged PATH contains /usr/local in $(basename "$privileged_script")"
    fi
done

contains "$installer" '--agent-token-stdin'
contains "$installer" '--agent-token is disabled because command-line secrets leak'
contains "$installer" 'Legacy --node-id/--api-key enrollment is disabled'
contains "$installer" 'validate_https_api_host "$existing_api_host"'
contains "$installer" 'Existing Agent.ApiHost is missing or does not use HTTPS'
contains "$installer" 'refusing to rotate its token or retarget the panel automatically'
contains "$installer" 'normalized_existing_host=$(https_api_origin "$existing_api_host")'
contains "$installer" 'normalized_supplied_host=$(https_api_origin "$API_HOST_ARG")'
contains "$installer" 'rewrite_agent_token /etc/znode/config.json "$updated_config" "$AGENT_TOKEN_ARG"'
contains "$installer" '"${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[4]}"'
contains "$installer" 'temporary_config=$(mktemp /etc/znode/config.json.XXXXXX)'
contains "$installer" 'mv -f "$temporary_config" "$config_file"'
contains "$installer" 'checksum_url="${asset_url}.dgst"'
contains "$installer" '/releases/tags/${version}'
contains "$installer" '/"digest": "sha256:/'
contains "$installer" '[[ "$expected" != "$actual" ]]'
contains "$installer" 'archive_size > 268435456'
contains "$installer" 'uncompressed_size > 536870912'
contains "$installer" 'find "$staging_directory" -type l'
contains "$installer" '(\.[0-9]+)?'
contains "$installer" 'staging_directory=$(mktemp -d'
contains "$installer" 'validate_geodata "$staging_directory"'
contains "$installer" 'install_geodata "$current_directory"'
contains "$installer" 'mv "$current_directory" "$previous_directory"'
contains "$installer" 'restore_previous_runtime'
contains "$installer" 'verify_runtime_checksum "$previous_directory"'
contains "$installer" 'if ! ensure_zboard_config_type; then'
contains "$installer" 'secure_znode_config_permissions'
contains "$installer" 'chown root:root "$config_file" && chmod 600 "$config_file"'
contains "$installer" 'LimitNOFILE=262144'
contains "$installer" 'TasksMax=8192'
contains "$installer" 'MemoryMax=90%'
contains "$installer" 'rollback_activated_runtime "$had_previous"'
contains "$installer" 'acquire_znode_operation_lock || exit 1'
contains "$installer" 'trap release_znode_operation_lock EXIT'
not_contains "$installer" 'if restore_previous_runtime; then'
contains "$installer" 'update được đánh dấu thất bại'
contains "$installer" 'manager_ref=$(latest_release_version "$RELEASE_REPO_ARG")'
contains "$installer" 'download_verified_script_asset "$RELEASE_REPO_ARG" "$manager_ref" znode.sh "$temporary"'
contains "$installer" 'temporary=$(mktemp /usr/bin/.znode.XXXXXX)'
contains "$installer" 'mv -f "$temporary" /usr/bin/znode'
contains "$manager" '/usr/local/znode.previous/.znode.sha256'
contains "$manager" "--proto '=https' --tlsv1.2"
contains "$manager" 'target_version="$1"'
contains "$manager" 'download_verified_script_asset "$release_repo" "$trusted_installer_ref" install.sh "$temporary"'
contains "$manager" 'bash "$temporary" "$target_version" "$@"'
contains "$manager" 'download_verified_script_asset "$release_repo" "$manager_ref" znode.sh "$temporary"'
contains "$manager" 'temporary=$(mktemp /usr/bin/.znode.XXXXXX)'
contains "$manager" 'mv -f "$temporary" /usr/bin/znode'
contains "$manager" 'return "$update_status"'
contains "$manager" 'validate_runtime_geodata /usr/local/znode.previous'
contains "$manager" 'install_runtime_geodata /usr/local/znode'
contains "$manager" 'restore_runtime_after_failed_rollback'
contains "$manager" 'rollback() ('
contains "$manager" 'acquire_znode_operation_lock || return 1'
contains "$manager" 'rm /usr/local/znode.previous/ -rf'
contains "$release_workflow" 'actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803'
contains "$release_workflow" 'persist-credentials: false'
contains "$release_workflow" 'openssl dgst -sha256 "$archive"'
contains "$release_workflow" '"${archive}.dgst"'
contains "$release_workflow" 'script/install.sh'
contains "$release_workflow" 'script/znode.sh'
contains "$release_workflow" '--verify-tag'

not_contains "$manager" 'bash <(curl'
not_contains "$manager" '--no-check-certificate'
not_contains "$manager" 'systemctl stop firewalld.service'
not_contains "$manager" 'ufw disable'
not_contains "$manager" 'iptables -P INPUT ACCEPT'
not_contains "$manager" '${release_repo}/${target_version}/script/install.sh'
not_contains "$manager" 'raw.githubusercontent.com'
not_contains "$installer" 'raw.githubusercontent.com'
not_contains "$installer" 'curl -sL "$url"'
not_contains "$installer" 'sed "s|^\([[:space:]]*\"AgentToken\"'
not_contains "$readme" "--agent-token 'AGENT_TOKEN'"

eval "$(extract_function_before "$installer" https_api_origin validate_https_api_host)"
[[ "$(https_api_origin 'https://Panel.Example:443/agent')" == 'https://panel.example' ]] \
    || fail 'HTTPS origin normalization did not normalize host/default port'
[[ "$(https_api_origin 'https://panel.example:8443/agent')" == 'https://panel.example:8443' ]] \
    || fail 'HTTPS origin normalization dropped a non-default port'
if https_api_origin 'http://panel.example' >/dev/null; then
    fail 'external HTTP ApiHost was accepted'
fi
if https_api_origin 'https://panel.example:70000' >/dev/null; then
    fail 'out-of-range HTTPS port was accepted'
fi

eval "$(extract_function_before "$installer" rewrite_agent_token validate_existing_agent_binding)"
test_directory=$(mktemp -d)
trap 'rm -rf "$test_directory"' EXIT
printf '{\n  "Agent": {\n    "AgentToken": "old",   \r\n    "PollInterval": 15\n  }\n}\n' > "$test_directory/config.json"
rewrite_agent_token "$test_directory/config.json" "$test_directory/updated.json" 'new-token' \
    || fail 'safe AgentToken line with trailing whitespace was rejected'
grep -Fq '"AgentToken": "new-token",' "$test_directory/updated.json" \
    || fail 'AgentToken rewrite dropped the JSON comma'
grep -Fq '"PollInterval": 15' "$test_directory/updated.json" \
    || fail 'AgentToken rewrite removed neighboring settings'
printf '{"AgentToken": "old", "PollInterval": 15}\n' > "$test_directory/inline.json"
if rewrite_agent_token "$test_directory/inline.json" "$test_directory/inline-updated.json" 'new-token'; then
    fail 'unsafe inline AgentToken rewrite was accepted'
fi

lock_functions="$(extract_function_before "$installer" acquire_znode_operation_lock release_znode_operation_lock)"
lock_functions+=$'\n'
lock_functions+="$(extract_function_before "$installer" release_znode_operation_lock validate_geodata)"
eval "$lock_functions"
red=''
plain=''
ZNODE_OPERATION_LOCK_FILE="$test_directory/operation.lock"
ZNODE_OPERATION_LOCK_DIRECTORY="${ZNODE_OPERATION_LOCK_FILE}.d"
ZNODE_OPERATION_LOCK_BACKEND=''
acquire_znode_operation_lock || fail 'could not acquire the first operation lock'
printf '%s\n' \
    '#!/usr/bin/env bash' \
    "$lock_functions" \
    'red=""' \
    'plain=""' \
    'ZNODE_OPERATION_LOCK_FILE="$1"' \
    'ZNODE_OPERATION_LOCK_DIRECTORY="${ZNODE_OPERATION_LOCK_FILE}.d"' \
    'ZNODE_OPERATION_LOCK_BACKEND=""' \
    'if acquire_znode_operation_lock; then release_znode_operation_lock; exit 0; fi' \
    'exit 1' > "$test_directory/lock-probe.sh"
if bash "$test_directory/lock-probe.sh" "$ZNODE_OPERATION_LOCK_FILE" >/dev/null 2>&1; then
    fail 'a concurrent operation acquired the active ZNode lock'
fi
release_znode_operation_lock
bash "$test_directory/lock-probe.sh" "$ZNODE_OPERATION_LOCK_FILE" >/dev/null 2>&1 \
    || fail 'operation lock was not reusable after release'

eval "$(extract_function_before "$manager" validate_release_version sha256_file)"
eval "$(extract_function_before "$manager" sha256_file latest_release_version)"
eval "$(extract_function_before "$manager" latest_release_version download_verified_script_asset)"
eval "$(extract_function_before "$manager" download_verified_script_asset run_installer)"
eval "$(extract_function_before "$manager" run_installer install)"
mkdir -p "$test_directory/mock-bin"
cat > "$test_directory/mock-bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -e
output=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) output="$2"; shift 2 ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
write_payload() {
  {
    printf '%s\n' '#!/usr/bin/env bash'
    printf '%s\n' 'printf '\''%s\n'\'' "$@" > "$RUN_INSTALLER_CAPTURE"'
    printf '# verified release fixture %.0s' {1..48}
    printf '\n'
  } > "$1"
}
if [[ "$url" == *"/releases/latest" ]]; then
  printf '{"tag_name":"v9.9"}\n'
elif [[ "$url" == *"/releases/tags/v9.9" ]]; then
  fixture=$(mktemp)
  write_payload "$fixture"
  digest=$(openssl dgst -sha256 "$fixture" | awk '{print tolower($NF)}')
  rm -f "$fixture"
  printf '  "name": "install.sh",\n  "digest": "sha256:%s"\n' "$digest"
else
  printf '%s\n' "$url" > "$RUN_INSTALLER_URL_CAPTURE"
  write_payload "$output"
fi
MOCK_CURL
chmod 0755 "$test_directory/mock-bin/curl"
release_repo='example/znode'
red=''
plain=''
export RUN_INSTALLER_URL_CAPTURE="$test_directory/installer-url"
export RUN_INSTALLER_CAPTURE="$test_directory/installer-args"
PATH="$test_directory/mock-bin:$PATH" run_installer v1.2 --release-repo example/znode \
    || fail 'trusted installer loader fixture failed'
grep -Fq '/releases/download/v9.9/install.sh' "$RUN_INSTALLER_URL_CAPTURE" \
    || fail 'explicit target selected its own historical installer'
[[ "$(sed -n '1p' "$RUN_INSTALLER_CAPTURE")" == 'v1.2' ]] \
    || fail 'explicit binary target was not passed as loader data'

manager_geodata_functions=$(sed -n '/^validate_runtime_geodata() {$/,/^swap_runtime_trees() {$/p' "$manager" | sed '$d')
eval "$manager_geodata_functions"
eval "$(extract_function_before "$manager" swap_runtime_trees start_znode_service)"
mkdir -p "$test_directory/runtime-geodata" "$test_directory/live-geodata"
dd if=/dev/zero of="$test_directory/runtime-geodata/geoip.dat" bs=1024 count=1 >/dev/null 2>&1
dd if=/dev/zero of="$test_directory/runtime-geodata/geosite.dat" bs=1024 count=1 >/dev/null 2>&1
printf 'old-ip\n' > "$test_directory/live-geodata/geoip.dat"
printf 'old-site\n' > "$test_directory/live-geodata/geosite.dat"
install_runtime_geodata "$test_directory/runtime-geodata" "$test_directory/live-geodata" \
    || fail 'rollback geodata transaction rejected valid release files'
cmp -s "$test_directory/runtime-geodata/geoip.dat" "$test_directory/live-geodata/geoip.dat" \
    || fail 'rollback did not activate geoip.dat from the selected runtime'
cmp -s "$test_directory/runtime-geodata/geosite.dat" "$test_directory/live-geodata/geosite.dat" \
    || fail 'rollback did not activate geosite.dat from the selected runtime'

mkdir -p "$test_directory/runtime-current" "$test_directory/runtime-previous"
printf 'current\n' > "$test_directory/runtime-current/marker"
printf 'previous\n' > "$test_directory/runtime-previous/marker"
swap_runtime_trees "$test_directory/runtime-current" "$test_directory/runtime-previous" \
    || fail 'runtime tree swap failed in an isolated transaction'
[[ "$(sed -n '1p' "$test_directory/runtime-current/marker")" == 'previous' ]] \
    || fail 'rollback did not activate the previous runtime tree'
[[ "$(sed -n '1p' "$test_directory/runtime-previous/marker")" == 'current' ]] \
    || fail 'rollback did not preserve the replaced runtime tree'

fixture_digest=$(printf '%064d' 0)
parsed_digest=$(printf 'SHA2-256= %s\n' "$fixture_digest" \
    | awk 'toupper($1) ~ /^SHA(2-)?256=$/ {print tolower($2); exit}')
[[ "$parsed_digest" == "$fixture_digest" ]] || fail 'workflow .dgst SHA-256 format was not parsed'

metadata=$(printf '  "name": "znode-linux-64.zip",\n  "digest": "sha256:%s"\n' "$fixture_digest")
metadata_digest=$(printf '%s\n' "$metadata" | awk -v asset='znode-linux-64.zip' '
    /"name":/ { selected = index($0, "\"" asset "\"") > 0 }
    selected && /"digest": "sha256:/ {
        value=$0
        sub(/^.*"digest": "sha256:/, "", value)
        sub(/".*$/, "", value)
        print tolower(value)
        exit
    }
')
[[ "$metadata_digest" == "$fixture_digest" ]] || fail 'GitHub asset SHA-256 metadata was not parsed'

validate_line=$(grep -nF 'validate_geodata "$staging_directory"' "$installer" | head -n 1 | cut -d: -f1)
activate_line=$(grep -nF 'if ! mv "$staging_directory" "$current_directory"' "$installer" | head -n 1 | cut -d: -f1)
install_geodata_line=$(grep -nF 'if ! install_geodata "$current_directory"' "$installer" | tail -n 1 | cut -d: -f1)
[[ "$validate_line" -lt "$activate_line" ]] || fail 'geodata must be validated before runtime activation'
[[ "$install_geodata_line" -gt "$activate_line" ]] || fail 'geodata must not mutate /etc before runtime activation'

migration_line=$(grep -nF '        if ! migrate_legacy_connection_profile; then' "$installer" | tail -n 1 | cut -d: -f1)
openrc_restart_line=$(grep -nF '            service znode restart' "$installer" | tail -n 1 | cut -d: -f1)
[[ "$openrc_restart_line" -gt "$migration_line" ]] || fail 'Alpine update must restart, not merely start, the service'

contains "$installer" '"DisableUDPContentSniffing": false'
not_contains "$installer" 's/"DisableUDPContentSniffing"[[:space:]]*:[[:space:]]*true/"DisableUDPContentSniffing": false/'

rollback_geodata_line=$(grep -nF 'if ! install_runtime_geodata /usr/local/znode; then' "$manager" | head -n 1 | cut -d: -f1)
rollback_start_line=$(grep -nF '    start_znode_service || true' "$manager" | head -n 1 | cut -d: -f1)
[[ "$rollback_geodata_line" -lt "$rollback_start_line" ]] || fail 'rollback geodata must be activated before service start'

echo 'security_scripts_test: ok'
