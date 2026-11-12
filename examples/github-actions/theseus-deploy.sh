#!/usr/bin/env bash
set -euo pipefail

: "${THESEUS_SSH_KEY:?Set the THESEUS_SSH_KEY secret}"
: "${THESEUS_KNOWN_HOSTS:?Set the THESEUS_KNOWN_HOSTS secret}"
: "${THESEUS_SSH_DEST:?Set the THESEUS_SSH_DEST variable}"
: "${GITHUB_REPOSITORY:?}"
: "${GITHUB_SHA:?}"
: "${GITHUB_RUN_NUMBER:?}"

# Restrict values before embedding them in the remote shell command.
[[ "$THESEUS_SSH_DEST" =~ ^([a-zA-Z0-9_][a-zA-Z0-9_.+-]*@)?[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]]
[[ "$GITHUB_REPOSITORY" =~ ^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$ ]]
[[ "$GITHUB_SHA" =~ ^[a-f0-9]{40}$ ]]
[[ "$GITHUB_RUN_NUMBER" =~ ^[1-9][0-9]*$ ]]
[[ "${GITHUB_REF:-}" == refs/heads/main ]]

remote="theseus receive --repository '$GITHUB_REPOSITORY' --commit '$GITHUB_SHA' --sequence '$GITHUB_RUN_NUMBER' --branch main"
if [[ -n "${THESEUS_IMAGE:-}" ]]; then
  [[ "$THESEUS_IMAGE" =~ ^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$ ]]
  remote+=" --image '$THESEUS_IMAGE'"
fi

umask 077
credentials=$(mktemp -d)
trap 'rm -rf "$credentials"' EXIT
printf '%s\n' "$THESEUS_SSH_KEY" > "$credentials/key"
printf '%s\n' "$THESEUS_KNOWN_HOSTS" > "$credentials/known_hosts"

ssh -F /dev/null -T -i "$credentials/key" \
  -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o "UserKnownHostsFile=$credentials/known_hosts" -o ConnectTimeout=30 \
  -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
  "$THESEUS_SSH_DEST" "$remote" < "${1:?Pass a release.tar.gz bundle}"
