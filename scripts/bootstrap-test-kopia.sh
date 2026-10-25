#!/usr/bin/env bash
#
# Boots a throwaway kopia server for the acceptance tests and prints the
# KOPIA_SERVER_* exports on stdout.
#
# A one-shot container creates a filesystem repository on a named volume, then
# the server container opens it with the UI password and CSRF protection on,
# exactly as a production `kopia server start` would run. Both share one
# hostname and connect as root@kopia-acc, so `current-user` and the policy
# targets of the snapshot sources agree.
#
# Things this script has to get right for kopia:
#
#   * boolean flags are passed bare (--insecure, --no-ui). Kopia's CLI parser
#     rejects --flag=true and --flag=false with "unexpected true/false".
#   * the port accepts TCP before kopia serves: Docker's port proxy answers at
#     once, and in TLS mode the listener is open while the key is generated.
#     Every curl therefore has a timeout, and readiness is the real CSRF flow:
#     GET / for a token, then GET /api/v1/repo/status until "connected": true.
#   * the in-memory TLS certificate (--tls-generate-cert) is regenerated on
#     every start, so its fingerprint is read again after a restart.
#   * the auth-cookie signing key is regenerated on every start too, which
#     invalidates every CSRF token; --restart exists to test exactly that.
#     Set KOPIA_AUTH_COOKIE_SIGNING_KEY to keep tokens valid across restarts.
#   * credentials reach the container as environment variables, not argv.
#
# Usage:
#   eval "$(scripts/bootstrap-test-kopia.sh)"                  # plain HTTP, CSRF on
#   eval "$(KOPIA_TEST_TLS=1 scripts/bootstrap-test-kopia.sh)" # HTTPS, kopia-generated certificate
#   eval "$(KOPIA_TEST_UI=0 scripts/bootstrap-test-kopia.sh)"  # --no-ui, for negative tests
#   eval "$(scripts/bootstrap-test-kopia.sh --restart)"        # restart: new signing key, stale tokens
#   scripts/bootstrap-test-kopia.sh --teardown                 # removes container, volume and state
#
# Settings, all optional:
#   KOPIA_TEST_IMAGE         image to test (default kopia/kopia:0.23.1; tags have no "v")
#   KOPIA_TEST_CONTAINER     container name (default kopia-tfprovider-test)
#   KOPIA_TEST_PORT          host port, bound to 127.0.0.1 only (default 51515)
#   KOPIA_TEST_TLS           1 for HTTPS with a kopia-generated certificate
#   KOPIA_TEST_UI            0 to start the server with --no-ui
#   KOPIA_TEST_EXTRA_ARGS    extra `kopia server start` flags, word-split
#   KOPIA_TEST_WAIT_SECONDS  how long to wait for the server (default 90)
#   KOPIA_TEST_STATE_DIR     where credentials and the certificate are kept
#
# Besides KOPIA_SERVER_URL, KOPIA_SERVER_USERNAME, KOPIA_SERVER_PASSWORD and,
# with TLS, KOPIA_SERVER_CERT_FINGERPRINT, the script exports what the
# acceptance tests need to know about the server: KOPIA_ACC_CA_FILE (the
# certificate as PEM, for testing ca_cert_file), KOPIA_ACC_CONTROL_USERNAME and
# KOPIA_ACC_CONTROL_PASSWORD (the server-control user, for negative tests),
# KOPIA_ACC_CONTAINER, KOPIA_ACC_VERSION (the image tag), KOPIA_ACC_UI, and
# KOPIA_ACC_SOURCE_HOST and KOPIA_ACC_SOURCE_USER (the connection identity).
#
# Beware: kopia's own client commands, such as `kopia server status`, read
# KOPIA_SERVER_USERNAME and KOPIA_SERVER_PASSWORD as the server-control
# credentials, so do not run them in a shell with these exports.
#
# On failure the script prints `false` on stdout, so that the eval above fails
# too instead of silently exporting nothing — `eval "$(...)"` otherwise returns
# 0 whatever the script's exit status was.
#
# Everything it creates is disposable; never point this at a real server.
set -euo pipefail

IMAGE="${KOPIA_TEST_IMAGE:-kopia/kopia:0.23.1}"
CONTAINER="${KOPIA_TEST_CONTAINER:-kopia-tfprovider-test}"
VOLUME="${CONTAINER}-data"
PORT="${KOPIA_TEST_PORT:-51515}"
TLS="${KOPIA_TEST_TLS:-0}"
UI="${KOPIA_TEST_UI:-1}"
EXTRA_ARGS="${KOPIA_TEST_EXTRA_ARGS:-}"
WAIT_SECONDS="${KOPIA_TEST_WAIT_SECONDS:-90}"
STATE_DIR="${KOPIA_TEST_STATE_DIR:-${TMPDIR:-/tmp}/${CONTAINER}}"
STATE_FILE="${STATE_DIR}/env"
CA_FILE="${STATE_DIR}/server-cert.pem"

# The hostname is lower-case without dots: kopia lower-cases the OS hostname
# and cuts it at the first dot.
SOURCE_HOST="kopia-acc"
SOURCE_USER="root"
UI_USER="tfacc"
CTRL_USER="server-control"

# The image tag names the kopia release under test, unless it is a moving tag
# such as "latest".
case "$IMAGE" in
  *:*) VERSION_TAG="${IMAGE##*:}" ;;
  *) VERSION_TAG="latest" ;;
esac

log() { echo "$*" >&2; }

# Traffic to the container must never go through HTTP(S)_PROXY or ALL_PROXY.
# The server may not be serving yet although the port accepts connections, so
# every request has a timeout.
curl() { command curl --noproxy '*' --connect-timeout 2 --max-time 10 "$@"; }

scheme() { if [[ "$TLS" == "1" ]]; then echo https; else echo http; fi; }
base_url() { echo "$(scheme)://127.0.0.1:${PORT}"; }

# curl only checks readiness here; certificate pinning is the provider's job.
curl_tls() { if [[ "$TLS" == "1" ]]; then echo "-k"; fi; }

rand_hex() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }

if [[ "${1:-}" == "--teardown" ]]; then
  docker rm -f -v "$CONTAINER" >/dev/null 2>&1 || true
  docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
  rm -rf "$STATE_DIR"
  log "removed $CONTAINER, volume $VOLUME and $STATE_DIR"
  exit 0
fi

trap 'status=$?; if [[ $status -ne 0 ]]; then echo false; fi' EXIT

command -v docker >/dev/null || { log "docker not found"; exit 1; }

container_exists() { docker inspect "$CONTAINER" >/dev/null 2>&1; }
container_running() {
  [[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || echo false)" == "true" ]]
}

# save_state keeps the credentials, so that a later run can reuse the server.
# $1 is the certificate fingerprint, empty without TLS.
save_state() {
  mkdir -p "$STATE_DIR"
  (
    umask 077
    {
      printf 'IMAGE=%q\nTLS=%q\nUI=%q\nPORT=%q\n' "$IMAGE" "$TLS" "$UI" "$PORT"
      printf 'UI_PASS=%q\nCTRL_PASS=%q\nREPO_PASS=%q\n' "$UI_PASS" "$CTRL_PASS" "$REPO_PASS"
      printf 'FINGERPRINT=%q\n' "${1:-}"
    } >"$STATE_FILE"
  )
}

create_repository() {
  log "creating a filesystem repository in volume $VOLUME"
  docker volume create "$VOLUME" >/dev/null
  # The image keeps its config, cache and logs under /app
  # (KOPIA_CONFIG_PATH=/app/config/repository.config and friends), so one
  # volume on /app holds them and the repository in /app/repo, which
  # `repository create filesystem` creates.
  KOPIA_PASSWORD="$REPO_PASS" docker run --rm \
    --hostname "$SOURCE_HOST" \
    -e KOPIA_PASSWORD \
    -v "${VOLUME}:/app" \
    "$IMAGE" \
    repository create filesystem \
    --path=/app/repo \
    --override-hostname="$SOURCE_HOST" \
    --override-username="$SOURCE_USER" \
    >&2
}

start_server() {
  local tls_args ui_args=""
  if [[ "$TLS" == "1" ]]; then
    # An in-memory self-signed certificate, valid for 127.0.0.1. Kopia prints
    # "SERVER CERT SHA256: <fingerprint>" and, with --tls-print-server-cert,
    # "SERVER CERTIFICATE: <base64 of the PEM>" to stderr.
    tls_args="--tls-generate-cert --tls-generate-rsa-key-size=2048 --tls-generate-cert-name=127.0.0.1 --tls-print-server-cert"
  else
    tls_args="--insecure"
  fi
  if [[ "$UI" == "0" ]]; then ui_args="--no-ui"; fi

  log "starting $IMAGE as $CONTAINER on $(base_url)"
  # `kopia server start` reads KOPIA_SERVER_USERNAME/_PASSWORD as the UI user
  # and KOPIA_SERVER_CONTROL_USER/_PASSWORD as the server-control user.
  # shellcheck disable=SC2086
  KOPIA_PASSWORD="$REPO_PASS" \
    KOPIA_SERVER_USERNAME="$UI_USER" \
    KOPIA_SERVER_PASSWORD="$UI_PASS" \
    KOPIA_SERVER_CONTROL_USER="$CTRL_USER" \
    KOPIA_SERVER_CONTROL_PASSWORD="$CTRL_PASS" \
    docker run -d \
    --name "$CONTAINER" \
    --hostname "$SOURCE_HOST" \
    -p "127.0.0.1:${PORT}:51515" \
    -e KOPIA_PASSWORD \
    -e KOPIA_SERVER_USERNAME -e KOPIA_SERVER_PASSWORD \
    -e KOPIA_SERVER_CONTROL_USER -e KOPIA_SERVER_CONTROL_PASSWORD \
    ${KOPIA_AUTH_COOKIE_SIGNING_KEY:+-e KOPIA_AUTH_COOKIE_SIGNING_KEY} \
    -v "${VOLUME}:/app" \
    "$IMAGE" \
    server start \
    --address=0.0.0.0:51515 \
    $tls_args $ui_args $EXTRA_ARGS \
    --shutdown-grace-period=2s \
    >/dev/null
}

check_deadline() {
  if ! container_running; then
    log "container $CONTAINER is not running; its logs follow"
    docker logs --tail 100 "$CONTAINER" >&2 || true
    exit 1
  fi
  if [[ "$(date +%s)" -ge "$1" ]]; then
    log "kopia was not ready after ${WAIT_SECONDS}s at $(base_url); its logs follow"
    docker logs --tail 100 "$CONTAINER" >&2 || true
    exit 1
  fi
}

# With --no-ui only the control API is served: no CSRF, server-control user.
wait_ready_control() {
  local deadline status
  deadline=$(($(date +%s) + WAIT_SECONDS))
  while :; do
    check_deadline "$deadline"
    # shellcheck disable=SC2046
    status="$(curl -sS $(curl_tls) -u "${CTRL_USER}:${CTRL_PASS}" "$(base_url)/api/v1/control/status" 2>/dev/null)" || true
    if grep -Eq '"connected": ?true' <<<"$status"; then
      log "ready: GET /api/v1/control/status reports connected"
      return 0
    fi
    sleep 1
  done
}

wait_ready() {
  local deadline code token status jar page
  if [[ "$UI" == "0" ]]; then
    wait_ready_control
    return 0
  fi
  deadline=$(($(date +%s) + WAIT_SECONDS))
  jar="${STATE_DIR}/cookies.txt"
  page="${STATE_DIR}/index.html"
  mkdir -p "$STATE_DIR"
  while :; do
    check_deadline "$deadline"

    # GET / as the UI user: the session cookie and the CSRF token.
    rm -f "$jar" "$page"
    # shellcheck disable=SC2046
    code="$(curl -sS $(curl_tls) -o "$page" -w '%{http_code}' -c "$jar" \
      -u "${UI_USER}:${UI_PASS}" "$(base_url)/" 2>/dev/null)" || true
    case "${code:-000}" in
      200) ;;
      401 | 403 | 404)
        log "GET / answered HTTP $code; kopia's logs follow"
        docker logs --tail 100 "$CONTAINER" >&2 || true
        exit 1
        ;;
      *)
        # 000: not listening or not serving yet.
        sleep 1
        continue
        ;;
    esac
    token="$(sed -n 's/.*<meta name="kopia-csrf-token" content="\([0-9a-f]\{64\}\)".*/\1/p' "$page" | head -n 1)"
    if [[ -z "$token" ]]; then
      log "GET / answered without a kopia-csrf-token meta tag"
      exit 1
    fi

    # A UI API call with Basic auth, the session cookie and the token.
    # shellcheck disable=SC2046
    status="$(curl -sS $(curl_tls) -b "$jar" -u "${UI_USER}:${UI_PASS}" \
      -H "X-Kopia-Csrf-Token: ${token}" "$(base_url)/api/v1/repo/status" 2>/dev/null)" || true
    if grep -Eq '"connected": ?true' <<<"$status"; then
      log "ready: GET /api/v1/repo/status reports connected"
      return 0
    fi
    sleep 1
  done
}

# fingerprint reads the certificate kopia generated at its last start from the
# container logs, saves it as PEM and prints its fingerprint; nothing without
# TLS.
fingerprint() {
  local fp check
  [[ "$TLS" == "1" ]] || return 0
  fp="$(docker logs "$CONTAINER" 2>&1 | sed -n 's/^SERVER CERT SHA256: \([0-9a-f]\{64\}\)$/\1/p' | tail -n 1)"
  if [[ -z "$fp" ]]; then
    log "no 'SERVER CERT SHA256:' line in the container logs"
    exit 1
  fi
  docker logs "$CONTAINER" 2>&1 | sed -n 's/^SERVER CERTIFICATE: //p' | tail -n 1 | base64 --decode >"$CA_FILE"
  if [[ ! -s "$CA_FILE" ]]; then
    log "no 'SERVER CERTIFICATE:' line in the container logs"
    exit 1
  fi
  if command -v openssl >/dev/null 2>&1; then
    check="$(openssl x509 -in "$CA_FILE" -outform DER | openssl dgst -sha256 -r | cut -d' ' -f1)"
    if [[ "$check" != "$fp" ]]; then
      log "fingerprint mismatch: kopia printed $fp, the certificate hashes to $check"
      exit 1
    fi
  fi
  echo "$fp"
}

print_exports() {
  printf 'export KOPIA_SERVER_URL=%q\n' "$(base_url)"
  printf 'export KOPIA_SERVER_USERNAME=%q\n' "$UI_USER"
  printf 'export KOPIA_SERVER_PASSWORD=%q\n' "$UI_PASS"
  if [[ -n "${1:-}" ]]; then
    printf 'export KOPIA_SERVER_CERT_FINGERPRINT=%q\n' "$1"
    printf 'export KOPIA_ACC_CA_FILE=%q\n' "$CA_FILE"
  else
    echo 'unset KOPIA_SERVER_CERT_FINGERPRINT KOPIA_ACC_CA_FILE'
  fi
  printf 'export KOPIA_ACC_CONTROL_USERNAME=%q\n' "$CTRL_USER"
  printf 'export KOPIA_ACC_CONTROL_PASSWORD=%q\n' "$CTRL_PASS"
  printf 'export KOPIA_ACC_CONTAINER=%q\n' "$CONTAINER"
  printf 'export KOPIA_ACC_VERSION=%q\n' "$VERSION_TAG"
  printf 'export KOPIA_ACC_UI=%q\n' "$UI"
  printf 'export KOPIA_ACC_SOURCE_HOST=%q\n' "$SOURCE_HOST"
  printf 'export KOPIA_ACC_SOURCE_USER=%q\n' "$SOURCE_USER"
}

# A server from an earlier run is reused when it was started from the same
# image in the same mode and its credentials are still known. Anything else is
# replaced: a leftover container built from another image would otherwise be
# tested in place of $IMAGE, silently, and with a green result.
reuse=""
if container_exists && [[ -f "$STATE_FILE" ]]; then
  saved_image="$(sed -n 's/^IMAGE=//p' "$STATE_FILE")"
  saved_mode="$(sed -n 's/^TLS=//p;s/^UI=//p;s/^PORT=//p' "$STATE_FILE" | tr '\n' ' ')"
  if [[ "$saved_image" == "$(printf '%q' "$IMAGE")" && "$saved_mode" == "$(printf '%q %q %q ' "$TLS" "$UI" "$PORT")" ]]; then
    reuse=1
  else
    log "replacing $CONTAINER: it was started from another image or in another mode"
  fi
fi

if [[ -n "$reuse" ]]; then
  # shellcheck disable=SC1090
  . "$STATE_FILE"
  if [[ "${1:-}" == "--restart" ]]; then
    log "restarting $CONTAINER; CSRF tokens issued before are now stale"
    docker restart -t 10 "$CONTAINER" >/dev/null
  elif ! container_running; then
    # Stopped after a Docker or host restart: the repository survives on the
    # volume, so start it again rather than waiting for a timeout.
    log "starting the stopped container $CONTAINER"
    docker start "$CONTAINER" >/dev/null
  else
    log "reusing $CONTAINER; run with --teardown for a fresh server"
  fi
else
  if [[ "${1:-}" == "--restart" ]]; then
    log "nothing to restart: no server from an earlier run"
    exit 1
  fi
  docker rm -f -v "$CONTAINER" >/dev/null 2>&1 || true
  docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
  rm -rf "$STATE_DIR"

  UI_PASS="ui-$(rand_hex)"
  CTRL_PASS="ctl-$(rand_hex)"
  REPO_PASS="repo-$(rand_hex)"

  docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull -q "$IMAGE" >&2
  create_repository
  start_server
fi

save_state ""
wait_ready
FINGERPRINT="$(fingerprint)"
save_state "$FINGERPRINT"
print_exports "$FINGERPRINT"
