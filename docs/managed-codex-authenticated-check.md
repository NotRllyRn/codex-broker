# Authenticated managed Codex check

Run this workflow when an operator asks to test a managed Codex upgrade PR,
alongside review of upstream changes and CI results. The operator signs into
their authorized test account once; the remaining checks run automatically. No
stored account secret or wait for token expiry is needed. Usage-window resets
are unrelated to token expiry.

Build the harness from a checkout containing it:

```sh
CGO_ENABLED=0 go build -o /tmp/managed-codex-check ./cmd/managed-codex-check
```

Build the candidate runtime image from the exact upgrade PR commit. For a
candidate image tagged `codex-broker:candidate` and version `0.165.0`, run:

```sh
docker run --name managed-codex-auth-check --read-only \
  --cap-drop ALL --security-opt no-new-privileges \
  --pids-limit 128 --memory 1g --cpus 1 \
  --tmpfs /tmp:rw,nosuid,nodev,mode=1777,size=128m \
  --mount type=bind,source=/tmp/managed-codex-check,target=/managed-codex-check,readonly \
  --entrypoint /managed-codex-check codex-broker:candidate \
  --codex /usr/local/bin/codex --version 0.165.0 --timeout 20m
```

Build the harness for the Docker host architecture when running remotely. Use a
unique container name per run. Do not mount production data, credentials, normal
Codex homes, or broker secrets. Only the isolated temporary home receives the
fresh sign-in credential. The harness deletes its own temporary directory on
exit; the container's tmpfs disappears when it exits. The stopped container can
be retained for its sanitized result logs.

The harness prints the verification URL and device code. Share them directly
with the operator, not in a public PR comment. Keep the process running while
they sign in. Checks continue as soon as Codex reports login completion; no chat
reply is required. The operator can still tell the agent they have signed in so
it checks the result. If the code expires, cancel the old run and start a fresh
one rather than starting overlapping logins. The overall timeout is configurable,
but increasing it does not extend an upstream device code's lifetime.

The check uses the broker's actual Go app-server client, adapter, and vault code:

1. Verify the exact CLI version and perform fresh device-code login.
2. Verify the ChatGPT identity, stop the CLI, capture `auth.json`, and save an
   encrypted checkpoint with a disposable test vault key.
3. Restore into a different home/process and send `account/read` with
   `refreshToken: true` to force renewal.
4. Stop the CLI and require an actual access-token or refresh-token change.
   A successful RPC with unchanged tokens is **inconclusive**, not a pass.
5. Save and reload the renewed encrypted credential, then restore into another
   fresh process. Verify the same identity and live quota reads.
6. Run the normal minimal window-pulse adapter, require a completed response,
   reread available quota, and checkpoint the final credential.

Only pass/fail summaries and token-change booleans are printed. Token values,
hashes, account identity, and the disposable encryption key are not printed.
Checkpoint storage is an isolated encrypted file, not the production SQL
promotion path; broker CI covers database integration. This covers device OAuth,
not the optional browser callback flow. The account needs available short and
weekly quota; the successful pulse consumes a small amount of quota.

Record the PR head SHA, exact CLI version, CI results, authenticated-check result,
and deployment/recovery readiness before marking the upgrade ready. Do not mark
a missing, failed, timed-out, or inconclusive check as passed. Keep workflow or
adapter fixes in a separate PR from the automation-owned version-bump branch.
