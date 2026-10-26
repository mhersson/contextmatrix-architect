#!/usr/bin/env bash
# svc.sh - manage the contextmatrix-architect systemd user service.
#
# Deployment mode: systemd --user (per-user service).
# -------------------------------------------------
# The architect is installed as a *user* unit under ~/.config/systemd/user,
# like the agent and chat backends: no root for install or update, and the
# service is scoped to one operator. It runs no containers, so nothing
# waits for docker.
#
# Config placement.
# -----------------
# ExecStart passes --config explicitly so systemd and the binary agree on
# one file. The path honors XDG_CONFIG_HOME and defaults to
# ~/.config/contextmatrix-architect/config.yaml - the file the binary reads
# without --config.
#
# State.
# ------
# An empty or absent state_dir resolves to
# ${XDG_STATE_HOME:-$HOME/.local/state}/contextmatrix-architect, which a
# user service can write. install whitelists that path in ReadWritePaths,
# creates it, and pins XDG_STATE_HOME in the unit, because the user manager
# may not carry this shell's value. An absolute state_dir in the config is
# whitelisted and created instead; the architect does not expand ~.
# /var/lib/contextmatrix-architect is the image's volume and not writable by
# a user service. The sandbox only lets the service write directories that
# exist when it starts, which is why install creates the directory. Rerun
# install after changing state_dir or XDG_STATE_HOME.
#
# ripgrep.
# --------
# The grep tool in code stages execs rg; glob takes fd, fdfind or rg. The
# unit inherits the user manager's PATH (systemctl --user show-environment),
# which can differ from this shell's. The architect logs a warning at
# startup when rg is missing and runs anyway.
#
# Hardening.
# ----------
# The generated [Service] section applies a baseline sandbox + resource
# limits + restart-backoff policy:
#   NoNewPrivileges, ProtectSystem=strict, ProtectHome=read-only,
#   PrivateTmp, PrivateDevices, ProtectKernelTunables,
#   ProtectKernelModules, ProtectControlGroups, LockPersonality,
#   MemoryDenyWriteExecute, RestrictRealtime, RestrictNamespaces,
#   RestrictAddressFamilies, SystemCallArchitectures,
#   SystemCallFilter - restrict syscall/fs/namespace surface.
#   MemoryMax, TasksMax, LimitNOFILE - bound resource usage.
#   Restart=on-failure, RestartSec + RestartSteps + RestartMaxDelaySec
#   - exponential backoff.
#   ReadWritePaths - narrow the filesystem to the resolved state_dir. The
#   path is prefixed with `-` so a missing dir does not block startup;
#   systemd skips it, so install creates state_dir up front.
#
# Subcommands:
#   install [--dry-run]  Write the unit file (or print to stdout), reload
#                        daemon, enable, and restart if already running.
#                        Both forms warn on stderr about a missing config,
#                        a state_dir the service cannot write and a missing
#                        rg; install creates state_dir first and goes ahead
#                        anyway, with a warning if it cannot.
#   uninstall            Stop, disable, and remove the service file.
#   start / stop / status
#   print                Print the generated unit file to stdout.
#   verify               Print the unit and run `systemd-analyze --user
#                        verify` + grep-check for expected directives.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SERVICE_NAME="contextmatrix-architect"
SERVICE_FILE="${HOME}/.config/systemd/user/${SERVICE_NAME}.service"
BINARY="${SCRIPT_DIR}/contextmatrix-architect"
CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/contextmatrix-architect/config.yaml"
IMAGE_STATE_DIR="/var/lib/contextmatrix-architect"

usage() {
    cat <<EOF
Usage: $(basename "$0") <subcommand>

Subcommands:
  install [--dry-run]  Generate the systemd user service file, reload daemon,
                       enable, and restart if running. --dry-run prints the
                       unit to stdout without touching systemd.
  uninstall            Stop, disable, and remove the service file
  start                Start the service
  stop                 Stop the service
  status               Show the service status
  print                Print the generated unit file to stdout
  verify               Print the unit and run systemd-analyze + grep checks
EOF
}

# resolve_state_base is the XDG base the service pins as XDG_STATE_HOME.
resolve_state_base() {
    printf '%s' "${XDG_STATE_HOME:-$HOME/.local/state}"
}

# configured_state_dir is read_state_dir with YAML null spellings treated as
# empty, as the binary does.
configured_state_dir() {
    local v
    v="$(read_state_dir)"
    case "${v}" in
        "~" | null | Null | NULL) v="" ;;
    esac
    printf '%s' "${v}"
}

# resolve_state_dir is the directory the service uses: the config's
# state_dir when set, else the XDG default the binary computes itself.
resolve_state_dir() {
    local configured
    configured="$(configured_state_dir)"
    if [ -n "${configured}" ]; then
        printf '%s' "${configured}"
    else
        printf '%s/contextmatrix-architect' "$(resolve_state_base)"
    fi
}

# read_state_dir extracts the state_dir value from the config so it can be
# whitelisted in ReadWritePaths. Strips a trailing inline comment,
# surrounding whitespace, and optional quotes. Empty if the file or key is
# absent.
read_state_dir() {
    [ -f "${CONFIG}" ] || return 0
    awk '
        /^state_dir:/ {
            sub(/^state_dir:[[:space:]]*/, "")
            sub(/[[:space:]]+#.*/, "")
            gsub(/^[[:space:]]+|[[:space:]]+$/, "")
            gsub(/^"|"$/, "")
            gsub(/^'"'"'|'"'"'$/, "")
            print
            exit
        }
    ' "${CONFIG}"
}

# warn_preflight reports what would keep the unit from working, without
# stopping the install.
warn_preflight() {
    if [ ! -f "${CONFIG}" ]; then
        echo "warning: ${CONFIG} not found; create it with 'mkdir -p $(dirname "${CONFIG}") && ${BINARY} config defaults > ${CONFIG} && chmod 600 ${CONFIG}' and set contextmatrix_url and api_key" >&2
    fi

    local state_dir what
    state_dir="$(resolve_state_dir)"
    what="state_dir"
    if [ -z "$(configured_state_dir)" ]; then
        what="XDG_STATE_HOME (state_dir is empty)"
    fi
    case "${state_dir}" in
        "${IMAGE_STATE_DIR}")
            echo "warning: ${what} resolves to ${IMAGE_STATE_DIR}, the image's volume, which a user service cannot write; set state_dir to an absolute path you can write, or unset XDG_STATE_HOME for the XDG default" >&2
            ;;
        /*)
            # systemd splits ReadWritePaths on whitespace.
            case "${state_dir}" in
                *[[:space:]]*)
                    echo "warning: ${what} gives '${state_dir}', which contains whitespace, which splits the ReadWritePaths entry; use a path without spaces" >&2
                    ;;
            esac
            if [ -e "${state_dir}" ]; then
                if [ ! -d "${state_dir}" ] || [ ! -w "${state_dir}" ]; then
                    echo "warning: ${what} gives '${state_dir}', which is not a directory this user can write" >&2
                fi
            fi
            ;;
        *)
            echo "warning: ${what} gives '${state_dir}', which is not absolute; ~ is not expanded and a relative path resolves under the read-only checkout; use an absolute path" >&2
            ;;
    esac

    if ! command -v rg >/dev/null 2>&1; then
        echo "warning: rg not found on this shell's PATH; the architect's grep tool fails without ripgrep, the service still starts" >&2
    fi
}

# generate_unit emits the systemd unit file contents to stdout.
generate_unit() {
    local rw_paths=""
    local state_dir
    state_dir="$(resolve_state_dir)"
    case "${state_dir}" in
        /*) rw_paths="-${state_dir//[%]/%%}" ;;
    esac

    # Pin the base so the service resolves the same default as this script.
    # Always quote the whole assignment so whitespace survives.
    local state_base env_line
    state_base="$(resolve_state_base)"
    env_line="XDG_STATE_HOME=${state_base//[%]/%%}"
    env_line="\"${env_line}\""

    cat <<EOF
[Unit]
Description=ContextMatrix Architect (blueprint backend)

[Service]
Type=simple
WorkingDirectory=${SCRIPT_DIR}
Environment=${env_line}
ExecStart=${BINARY} serve --config ${CONFIG}
KillMode=mixed
TimeoutStopSec=60

# --- Sandboxing ---
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
# The architect writes only its state_dir. Paths are prefixed with '-' so a
# missing dir does not block startup; systemd skips it, so install creates
# state_dir before the unit starts.
ReadWritePaths=${rw_paths}
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictNamespaces=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources

# --- Resource limits ---
MemoryMax=2G
TasksMax=1024
LimitNOFILE=65536

# --- Restart policy ---
Restart=on-failure
RestartSec=10
RestartSteps=5
RestartMaxDelaySec=300

[Install]
WantedBy=default.target
EOF
}

# EXPECTED_DIRECTIVES is the set of hardening lines that must appear in
# the generated unit. `verify` grep-asserts each one; keep in sync with
# generate_unit. The ReadWritePaths entry is asserted as a substring
# because the state_dir suffix is dynamic; the Environment line likewise.
EXPECTED_DIRECTIVES=(
    "NoNewPrivileges=yes"
    "ProtectSystem=strict"
    "ProtectHome=read-only"
    "PrivateTmp=yes"
    "PrivateDevices=yes"
    "ProtectKernelTunables=yes"
    "ProtectKernelModules=yes"
    "ProtectControlGroups=yes"
    "LockPersonality=yes"
    "MemoryDenyWriteExecute=yes"
    "RestrictRealtime=yes"
    "RestrictNamespaces=yes"
    "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX"
    "SystemCallArchitectures=native"
    "SystemCallFilter=@system-service"
    "SystemCallFilter=~@privileged @resources"
    "MemoryMax=2G"
    "TasksMax=1024"
    "LimitNOFILE=65536"
    "Restart=on-failure"
    "RestartSec=10"
    "RestartSteps=5"
    "RestartMaxDelaySec=300"
    "ReadWritePaths=-/"
    'Environment="XDG_STATE_HOME='
)

cmd_install() {
    local dry_run=0
    if [ "${1:-}" = "--dry-run" ]; then
        dry_run=1
    fi

    warn_preflight

    if [ "${dry_run}" -eq 1 ]; then
        generate_unit
        return 0
    fi

    # Idempotent re-install: if the service is currently active, stop it
    # before overwriting the unit file and restart afterwards.
    local was_active=0
    if systemctl --user is-active --quiet "${SERVICE_NAME}"; then
        was_active=1
        systemctl --user stop "${SERVICE_NAME}"
    fi

    # A missing '-' ReadWritePaths entry is skipped under ProtectHome, so
    # the directory must exist before the service starts.
    local state_dir
    state_dir="$(resolve_state_dir)"
    case "${state_dir}" in
        /*)
            # 0750 like the service's own mkdir; the dir holds architect.db.
            if ! (umask 027 && mkdir -p "${state_dir}"); then
                echo "warning: could not create state_dir '${state_dir}'; create it and make it writable by this user before start" >&2
            fi
            ;;
    esac

    mkdir -p "$(dirname "${SERVICE_FILE}")"
    generate_unit > "${SERVICE_FILE}"
    echo "Service file written to ${SERVICE_FILE}"

    systemctl --user daemon-reload
    systemctl --user enable "${SERVICE_NAME}"

    if [ "${was_active}" -eq 1 ]; then
        systemctl --user start "${SERVICE_NAME}"
        echo "Service '${SERVICE_NAME}' restarted with new unit."
    else
        echo "Service '${SERVICE_NAME}' enabled. Run '$(basename "$0") start' to start it."
    fi
}

cmd_uninstall() {
    systemctl --user stop "${SERVICE_NAME}" 2>/dev/null || true
    systemctl --user disable "${SERVICE_NAME}" 2>/dev/null || true
    if [ -f "${SERVICE_FILE}" ]; then
        rm -f "${SERVICE_FILE}"
        echo "Removed ${SERVICE_FILE}"
    fi
    systemctl --user daemon-reload
    echo "Service '${SERVICE_NAME}' uninstalled."
}

cmd_start() {
    systemctl --user start "${SERVICE_NAME}"
}

cmd_stop() {
    systemctl --user stop "${SERVICE_NAME}"
}

cmd_status() {
    systemctl --user status "${SERVICE_NAME}"
}

cmd_print() {
    generate_unit
}

cmd_verify() {
    local unit
    unit="$(generate_unit)"
    echo "---- generated unit ----"
    printf '%s\n' "${unit}"
    echo "---- end unit ----"

    local missing=0
    for directive in "${EXPECTED_DIRECTIVES[@]}"; do
        if ! printf '%s\n' "${unit}" | grep -qF -- "${directive}"; then
            echo "MISSING: ${directive}" >&2
            missing=1
        fi
    done
    if [ "${missing}" -ne 0 ]; then
        echo "verify: one or more expected directives are missing" >&2
        return 1
    fi
    echo "verify: all expected directives present"

    if command -v systemd-analyze >/dev/null 2>&1; then
        local tmp
        tmp="$(mktemp --tmpdir "cm-architect-verify-XXXXXX.service")"
        trap 'rm -f "${tmp}"' RETURN
        printf '%s\n' "${unit}" > "${tmp}"
        echo "---- systemd-analyze --user verify ----"
        # --user verifies against the user manager's search path; it is
        # expected to print warnings for unresolved dirs on hosts that
        # lack them, but exits 0 when the unit is syntactically valid.
        if systemd-analyze --user verify "${tmp}"; then
            echo "systemd-analyze: ok"
        else
            echo "systemd-analyze: reported issues (non-fatal for syntax-only check)" >&2
        fi
    else
        echo "systemd-analyze not found; skipping unit verify"
    fi
}

if [ $# -eq 0 ]; then
    usage
    exit 1
fi

case "$1" in
    install)   shift; cmd_install "$@" ;;
    uninstall) cmd_uninstall ;;
    start)     cmd_start ;;
    stop)      cmd_stop ;;
    status)    cmd_status ;;
    print)     cmd_print ;;
    verify)    cmd_verify ;;
    *)
        echo "Unknown subcommand: $1" >&2
        echo "" >&2
        usage >&2
        exit 1
        ;;
esac
