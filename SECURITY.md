# Security policy

## Reporting a vulnerability

Do not open a public issue containing exploit details, credentials, or a
minimal proof of concept. Prefer GitHub's private vulnerability reporting for
this repository. If private reporting is unavailable, contact the repository
owner through the GitHub profile without publishing technical details in a
public thread.

Include the affected WEDRA version, platform, reproduction steps, and the
expected impact. Maintainers will acknowledge a complete report when they can
and will coordinate disclosure timing with the reporter.

## Supported line

The current development line is the only line receiving security fixes until
a support policy is published. Historical releases and compatibility aliases
are not automatically patched. The current product version is in `VERSION`;
the protocol version is in `protocol/VERSION`.

## Plugin and secret handling

Plugins are subprocesses and may run with the permissions of the current user.
Review `permissions.network`, `permissions.filesystem`, and
`permissions.secrets` before installing a plugin. Never put real API keys in
YAML, fixtures, logs, examples, or pull requests.

MCP path checks are a reference and path policy, not an operating-system
sandbox. Do not treat an MCP client or a plugin manifest as a security boundary
without separate OS-level isolation.

## Supply chain: what a release artifact actually proves

A release page is not a chain of custody by itself. This section states what is
signed, by whom, and — just as important — what a signature here does **not**
prove.

**What is in a release.** Binaries for six platforms (`wedra-*`, legacy
`tool-*`, `wedragui-windows-*`), a conformance archive, `SHA256SUMS`,
`sbom.spdx.json`, and two Sigstore bundles `SHA256SUMS.sigstore.json` and
`sbom.spdx.json.sigstore.json`. GitHub provenance attestations cover
`dist/*`, including the bundles.

**What is signed.** `SHA256SUMS` and `sbom.spdx.json`, with **keyless**
signing: Sigstore Fulcio issues a short-lived certificate against the GitHub
Actions OIDC token of the release job (`id-token: write`, already required for
the provenance attestation), and the signing event is written to the public
Rekor transparency log. Consequences worth stating plainly:

- There is **no signing key in this repository and no secret**. Nothing has to
  be configured, and a contributor who can trigger a release signs with the
  token the runner already holds under that job's own permissions — no more
  and no less.
- A verifier does not take our word on a shared secret; they check a Fulcio
  certificate and a public log entry.

**What a verifier must pin.** The certificate identity, exactly:

```
https://github.com/wykserdex/wedra/.github/workflows/release.yml@refs/tags/vX.Y.Z
```

with issuer `https://token.actions.githubusercontent.com`. This is the
`job_workflow_ref`-derived SAN that Fulcio puts in the certificate, and for a
reusable workflow `job_workflow_ref` names the **called** workflow
(`release.yml`), not the caller (`ci.yml`). Pinning it exactly is the control:
a signature from another branch, another repository, or a fork fails.

**Closing the loop.** Verifying `SHA256SUMS` covers every binary listed in it,
so `sha256sum --check --strict SHA256SUMS` after the signature check is what
turns "a signed list" into "these are the binaries". The release workflow does
both, in that order, in the same run; full instructions for a person are in
[`docs/supply-chain.md`](docs/supply-chain.md).

**Fail-closed by construction.** The verification step names the two signed
files explicitly. Discovering them by globbing `dist/*` would pass on zero
bundles — i.e. the signing step could vanish and the check would stay green.
Missing `cosign` fails the step with a message about the missing signature,
not a bare `command not found`.

**Two automated checks, on purpose.** The `cosign verify-blob` step above
proves a signature is good. It does not prove the signing step still *exists*
— deleting it breaks nothing anywhere. So `wedra check` step `supply` (and
therefore CI) also requires, statically: every `uses:` in
`.github/workflows/*.yml` pinned to a full 40-character SHA, and `release.yml`
still containing `sigstore/cosign-installer`, `cosign sign-blob`,
`cosign verify-blob`, both signed filenames in the signing and verification
steps, and the `command -v cosign` guard. `supply` verifies no cryptography;
that division of labour is the point.

**Toolchain pinning.** The cosign version is pinned explicitly
(`cosign-release: 'v3.1.3'`) rather than left to the installer default, which
is v3.0.6. v3.1.3 carries the fix for **GHSA-fx35-mq7g-6g98**, a verification
bypass via an unexpected public key in a legacy bundle. Verifying a signature
with a vulnerable verifier is not verifying it. Dependabot updates pinned
actions but not that input value; cosign must be bumped by hand.

**Status, stated honestly.** The signing and verification steps are in the
release workflow, but **no release has yet been produced through them**: a
keyless signature needs a runner OIDC token and cannot be reproduced locally.
The workflow YAML, the bash, the cosign flags (`--bundle`, `--yes`,
`--certificate-identity`, `--certificate-oidc-issuer`, cross-checked against
cosign v3.1.3 sources) and the certificate identity (cross-checked against
Fulcio's `pkg/identity/github/principal.go`) were verified statically, and the
pinned action SHAs against the GitHub API. **A CI run of the release job is
still required** to confirm that Fulcio issues for this repository and that the
public Sigstore instance is reachable from a runner. Until then, treat
"releases are signed" as intended behaviour, not as a shipped fact.

**What signing does not buy.** It does not protect the download itself (that is
HTTPS plus the URL you typed), it does not attest that the build is correct
(that is the provenance attestation, a separate object), and it does not make
the build reproducible. Comparing a signed artifact against one you built
yourself is a separate exercise, not automated here.

## Trust is granted by the kernel, not by the plugin
## The GUI HTTP server (`wedra gui`, `wedragui`, the `wedra mcp` gate console)

The GUI is an HTTP server that runs step plugins, so its port is a real
attack surface on the machine. What it enforces:

- **Session on everything.** Every path under `/api/*` requires the human's
  session cookie — reads included: run journals, step inputs and outputs, plugin
  lists, pipelines, gate state. Two paths are open and are listed in one place in
  the code (`publicAPI`): `GET /api/health` (version and liveness, no run data)
  and `/api/session` (the code exchange itself). `--no-session` turns the
  requirement off and is refused on a non-loopback address.
- **Host allow-list.** Requests are served only for `127.0.0.1`, `localhost`,
  `[::1]`, the address the operator passed, and any `--public-host`. A foreign
  `Host` is refused with `E_HOST_NOT_ALLOWED`. This is what stops DNS
  rebinding: a page on someone else's domain resolving to `127.0.0.1` sends its
  own `Host` header, and that header is now checked.
- **No implicit proxies.** `X-Forwarded-Host` is never read.
  `X-Forwarded-Proto` is read only under an explicit `--trusted-proxy`, and only
  to set the `Secure` cookie flag — the value cannot come from the client that
  is being judged.
- **Origin against the server's own identity.** The `Origin`/`Referer` check on
  mutations compares against the configured allow-list and external scheme, not
  against a value taken from the same request (which would be a tautology).
- **Entry by one-time code.** The server prints a short, single-use pairing code.
  It exchanges for a cookie whose value is a separate token with a 12-hour TTL
  held in server memory; the code itself dies on first use. The cookie name
  carries the port, so two instances on `localhost` cannot overwrite each
  other's session.
- **Listening beyond loopback is explicit.** `--listen` on a non-loopback
  address is refused unless `--allow-remote` is given, `0.0.0.0`/`::` additionally
  requires at least one `--public-host`, and `--no-session` is refused there
  outright. Pipelines are written with mode `0600` on POSIX.

What this does **not** do: it does not stop a process running as the same OS
user that can read the terminal, the browser profile, or the server's memory.
The session protects against an agent (or any process) that can only send HTTP
to the local port. TLS is not provided: put a reverse proxy in front and declare
it with `--trusted-proxy --public-host=...`, or keep the server on loopback.

## Untrusted plugin code (`sandbox: untrusted`)

A plugin is **trusted** — meaning it runs with the permissions of the current
user, with no isolation — only when the kernel has confirmed it. Confirmation
requires **both** the plugin `id` and the **sha256 of the plugin directory's
contents** to appear in the allow-list:

- the operator's own `wedra-trust.yaml`, looked up in this order:
  `--trust-config=<file>`, then `$WEDRA_TRUST_CONFIG`, then the directory of the
  binary (`%ProgramFiles%\wedra\wedra-trust.yaml`). The working directory is
  deliberately **not** searched: it is chosen by whoever invoked the command, not
  by the operator. A repository clone carrying its own `wedra-trust.yaml` passes
  every ownership and permission check (the file belongs to the invoking user,
  mode 0644) and would silently widen the allow-list. If such a file exists in
  the working directory but was not selected, the core prints a warning with the
  exact flag to use and proceeds without it;
- the built-in allow-list, generated from the commit pins in `registry.yaml`
  (`internal/plugin/trustseed_gen.go`, regenerate with
  `go run ./internal/plugin/cmd/genseed`).

The manifest can only **lower** trust, never raise it. `sandbox: untrusted` in a
manifest makes the plugin untrusted even when its hash is allow-listed, and
`sandbox: trusted` grants nothing at all. Plugins under `agent-plugins/` are
untrusted by construction regardless of any allow-list entry.

This is the direction the decision was previously pointing the wrong way in:
trust used to be read from the plugin's own `sandbox` field, which is written by
the plugin author. No manifest in this repository set that field, and
`internal/registry` did not know it existed — so a malicious community plugin
simply omitted the line and ran with full user privileges.

Because trust is content-addressed, tampering is caught even when the `id` is
unchanged: editing, replacing, or adding a file inside an installed plugin
revokes trust. A plugin directory that cannot be read, contains a symlink, or
does not exist is untrusted as well — `sandbox: untrusted` was never the only
thing that could disqualify a plugin.

`GET /api/plugins` reports the effective `trusted` flag, the `content_sha256` it
computed, and a `blocked_reason` for **every** untrusted plugin (not only
agent-written ones), so the hash for an allow-list entry is observable rather
than something to compute blind.

### Untrusted plugin code

An untrusted plugin is refused unless the operator explicitly opts in with
`--allow-untrusted-plugins`, and even then it only runs inside an OS-level
sandbox. A `untrusted` plugin may not declare `permissions.secrets`.

| Platform | Backend | Notes |
| --- | --- | --- |
| Linux | `bwrap` (bubblewrap) | read-only host filesystem (including `/tmp` and the plugin directory), separate PID/IPC/UTS, a private writable scratch directory created per run and deleted afterwards, **network namespace always separate**. A plugin that declares `any_host` gets egress through a userspace stack (`slirp4netns`); without it there is no network at all, and without `slirp4netns` installed the run is refused |
| macOS | none | fail-closed: backend archived, see below |
| Windows | none | fail-closed: untrusted plugins cannot run (blocked on low-integrity sandbox root, see below) |

A plugin that writes into its own directory revokes its own trust after the
first run, because the write changes the content hash. This is intended, and it
is the same reason the Linux backend mounts the plugin directory read-only.

### macOS: backend archived

The `sandbox-exec` backend was removed from the build and archived out of the
tree: it lives in git history (commit `9d17120`, files `sandbox_darwin.go` and
its behavioural test) and is not compiled or tested. macOS now resolves through the same
fail-closed path as Windows: an untrusted plugin is refused rather than run
without isolation.

The reason is integrity, not egress. `sandbox-exec` cannot express a read-only
bind mount, so the backend had to open the plugin directory for write, and a
plugin could rewrite itself and persist on disk. The Linux backend states the
opposite as a property it provides; on macOS that property did not hold. Note
also that the refusal message no longer points macOS users at `sandbox-exec`,
since recommending an isolator that is not wired up would be worse than saying
nothing.

Restoring it is a revert, but it should not happen before there is an answer for
the writable plugin directory. The archived test is the only behavioural
verification of a real isolator on real hardware in this repository, so it is
kept rather than deleted.

An AppContainer backend for Windows was investigated and is not enabled. This
section previously blamed host ACL state. That diagnosis was wrong, and the
measurements behind it probed the wrong operation. Corrected findings, all
measured on a clean Windows 10 Enterprise 22H2 x64 VM from a non-elevated
process (`elevated: false`), using `SetNamedSecurityInfo` with
`PROTECTED_DACL_SECURITY_INFORMATION`:

- A non-elevated process **can** create a directory and restrict its DACL to
  `SYSTEM` + `Administrators` + the container SID. Verified in three
  placements: `%LOCALAPPDATA%\Temp`, a nested subdirectory of it, and
  `C:\ProgramData`. All three returned `OK`.
- The restriction is effective: the acting user's own **unelevated** token is
  locked out of the directory immediately afterwards (`ReadDir` →
  `Access is denied`). The user's SID is gone from the DACL entirely, which is
  what is asserted by the test. Note the scope: an *elevated* token, or one whose
  `Administrators` SID is enabled rather than deny-only, still gets in through
  the `Administrators` ACE — and it could reclaim the directory regardless, so
  that is the correct outcome and not a gap. The `C:\ProgramData`
  `ACCESS_DENIED` previously recorded for a "DACL-only write" does not reproduce
  for a directory the process created itself; it applies to rewriting the DACL
  of an existing subtree.
- `PROTECTED_DACL_SECURITY_INFORMATION` is load-bearing. Without it the new ACL
  merges with inherited ACEs (6 ACEs instead of 3) and the user token still
  passes. With it, inheritance is severed and the user token is excluded.
- `SetNamedSecurityInfo` on `%LOCALAPPDATA%` itself succeeds in seconds. The
  previously recorded indefinite block did not reproduce; it looks like a slow
  operation rather than a hang, and the screen-idle symptom remains unexplained.
- **Rewriting the DACL of `%LOCALAPPDATA%` is not merely unnecessary, it is
  destructive.** Doing it locks the user out of their entire profile and breaks
  unrelated tooling (during this work it broke `go build`, whose scratch
  directory lives in `%LOCALAPPDATA%\Temp`). Any Windows backend must therefore
  restrict only directories it created itself.

The real remaining blocker is not the integrity level. That diagnosis was wrong as
well, and it came from a probe that never created an AppContainer process at
all. Measured on a **different** host — Windows 10 build 19041, non-elevated
(`elevated: false`), *not* the Enterprise 22H2 VM the measurements above come from
— a low-integrity AppContainer token wrote into medium-integrity directories with
no SACL change, no `WRITE_OWNER` and no elevated step:

- `CreateAppContainerProfile` plus `CreateProcessW` with
  `PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES` (`0x00020009`) started a real
  interpreter: CPython 3.14.4 ran to completion inside the container.
- Granted a single read+execute / modify ACE to the container SID, writes into
  medium-integrity directories **succeeded**. The mandatory label is not the gate.
  Per Microsoft's AppContainer documentation the permitted access is the
  *intersection* of the user/group grants and the AppContainer grants, and a
  medium-or-lower label does not veto a DACL grant — so the earlier
  "write-up is refused" result has no mechanism behind it.
- Eleven of eleven behavioural checks matched expectations: the plugin directory
  was readable but neither writable nor modifiable (no self-rewrite, which is the
  property the Linux backend provides via `--ro-bind`), only the granted scratch
  was writable, a directory with no grant was unreachable, an outbound connection
  was refused with `PermissionError`.

So a Windows backend is implementable by a non-elevated process, with no
install-time or elevated step. What the previous text called a blocker is a set
of ordinary implementation requirements, all of which were satisfied:

- **The runtime must belong to the sandbox.** A per-user Python under
  `%LOCALAPPDATA%` stays unreachable even with a read+execute grant on every
  ancestor directory — `CreateProcessW` answers `ERROR_PATH_NOT_FOUND`. A full
  installation is 2.3 GB; its core alone (interpreter, `DLLs/`, stdlib, without
  `site-packages`) is 33 MB and is what the sandbox should use. Granting read on
  the user's own Python would hand the container the user's site-packages too.
- **The environment block needs `USERPROFILE` and `LOCALAPPDATA`.** AppContainer
  derives the profile path from them. A syntactically valid block missing either
  fails with `ERROR_ENVVAR_NOT_FOUND`, and the *same* block is accepted outside a
  container, which makes the failure look inexplicable otherwise.
- **`PYTHONDONTWRITEBYTECODE=1`**, or the interpreter cannot import from a
  read-only plugin directory, because it writes `__pycache__` beside the modules.
- **Egress is decided by capabilities.** With none requested, outbound
  connections fail. Omitting `internetClient` is the mechanism, not a side effect.
- **ACL writes must go through `golang.org/x/sys/windows`** (`ACLFromEntries` plus
  `SetNamedSecurityInfo`, as in `internal/plugin/windows_acl_test.go`). A
  hand-written `SetEntriesInAclW` path returned success while adding no ACE at
  all: `UNPROTECTED_DACL_SECURITY_INFORMATION` is `0x20000000`, and with the
  wrong constant the DACL write is silently discarded.
- **Cleanup must not live only in deferred functions.** A non-zero exit path skips
  defers and leaks one AppContainer profile per run.

What this is **not**: a verified boundary for hostile code. The probe ran `cmd.exe`
and `python -c`, not a plugin. The loopback check proved nothing — nothing was
listening on the target port, and `ConnectionRefusedError` is what both a closed
sandbox and an absent listener produce. Read isolation, per-destination egress
filtering and resource limits are still absent; the last needs job objects, which
are **not** a security boundary (`JobObjectSecurityLimitInformation` was removed
in Windows Vista, `JOB_OBJECT_SET_SECURITY_ATTRIBUTES` is unsupported) — they
bound CPU, memory and process count and nothing else.

Windows therefore stays fail-closed: the backend is unwritten and
`sandbox_other.go` returns `ErrSandboxUnsupported`. The reason is now "not
implemented", not "the platform forbids it". The measurements above are not
reproducible from this tree — so they have been re-measured, and what follows
replaces them where they were wrong or silent.

### Re-measured (Windows, non-elevated, reproducible from this tree)

`internal/plugin/appcontainer_probe_test.go`, opt-in via
`go test -tags appcontainer ./internal/plugin -run TestAppContainerProbe -v`.

**Works, measured on this host:**

- `CreateAppContainerProfile` succeeds for a non-elevated process.
- `SetNamedSecurityInfo` succeeds — but only as
  `DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION`.
  `PROTECTED_DACL_SECURITY_INFORMATION` **alone** returns
  `ERROR_ACCESS_DENIED`, and that reads as "the platform forbids ACL writes".
  It is a modifier, not a request; the earlier text in this file mentioned it
  without saying it must be combined.
- The ACL must contain `SYSTEM` + `Administrators` + the container SID. An ACL
  with only the target entry is rejected with the same `ERROR_ACCESS_DENIED`.
  With `PROTECTED` the acting user is locked out of the directory, which is the
  intended property, and it is also why a probe must keep the user SID if the
  parent process still needs to write there.
- `windows.NewProcThreadAttributeList` creates a list; `sizeof(STARTUPINFOEX)`
  is 104 as expected.

**Does not work, measured on this host — this is the blocker:**

- `CreateProcessW` with `PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES` returns
  `ERROR_INVALID_PARAMETER`. The same executable launches through the same
  call with no attribute list, so the attribute is what is rejected. The cause
  was **not** isolated: ruled out by measurement were the flag arithmetic
  (`CREATE_UNICODE_ENVIRONMENT` added), the environment block, the working
  directory, `STARTUPINFOEX.cb`, the profile SID, and moving `SECURITY_CAPABILITIES`
  off the Go stack onto the heap. The probe skips rather than fails, so CI
  stays green and the blocker stays written down.

**Two traps that cost time and look like anything else:**

- `os/exec` cannot express this at all. `syscall.StartProcess` on Windows calls
  `newProcThreadAttributeList(2)` and supports only
  `PROC_THREAD_ATTRIBUTE_PARENT_PROCESS`; there is no `SysProcAttr` field for
  `SECURITY_CAPABILITIES`. A backend needs its own `CreateProcessW` path and
  therefore its own plumbing for stdio, which is a much larger change than
  adding one platform file.
- Store app execution aliases (here the `python` on `PATH` resolves to
  `C:\Program Files\PyManager\python.exe`) cannot be launched through
  `CreateProcessW`; they activate through a different mechanism and the call
  returns `ERROR_INVALID_PARAMETER`. Measured against the real interpreter
  under `%LOCALAPPDATA%\Python`, it launches normally.

A hand-rolled `NewLazySystemDLL(...).NewProc("InitializeProcThreadAttributeList")
.Call(...)` also faulted with an access violation on this host, while the same
operation through `windows.NewProcThreadAttributeList` succeeds. Use the
package's constructor; do not hand-roll this path.

### What a Windows user can do today: trust their own code

Fail-closed applies to code that is **not** in the allow-list. A plugin that is
in the allow-list runs with user rights and needs no isolator, on Windows
included. That makes the editor, the importer and the store usable on Windows
for the case they exist for — a person writing their own plugin:

1. write the plugin (editor, importer, or a text editor);
2. it is not trusted — its content hash differs from any allow-list entry, and
   an isolator does not exist on this platform, so a run is refused;
3. read the new hash from `GET /api/plugins/<id>/source` (`content_sha256`);
4. add `"<id>@<sha256:…>"` to `trusted_plugins` in `wedra-trust.yaml`;
5. it runs.

Step 4 is a deliberate decision by a person, and it is the whole point of
trusting by content hash rather than by name. The editor shows the exact line to
copy and the path the running server actually reads, because the file is
deliberately **not** read from the working directory: a clone of a repository
with an appended `wedra-trust.yaml` once passed every check while silently
widening the allow-list.

`GET /api/trust` reports the search order (`--trust-config`, then
`WEDRA_TRUST_CONFIG`, then the binary directory) and whether the file exists. An
empty `config_path` is itself an answer: only the flag or the variable can
configure trust.

This route does **not** make third-party code safe on Windows. It grants user
rights to code the operator has chosen, which is the same trust model as every
other install-and-trust tool, and it is not a boundary against hostile code.
That boundary remains the AppContainer backend above, which is implementable and
still unwritten.

If the backend is missing, or the host forbids creating one, the run stops
with `platform:sandbox_unavailable` before any process is created. The
availability probe runs once per process: `bwrap` is verified by actually
building a namespace, `sandbox-exec` by actually writing a probe file under the
real profile. A backend that is installed but cannot isolate anything is treated
as absent. There is no path that executes untrusted code outside a sandbox.

What the backends do **not** provide: read isolation (a plugin can read any
file the user can read, subject to the host mount set), egress filtering when a
plugin declares network permissions, and resource limits. Do not treat
`--allow-untrusted-plugins` as a complete boundary for hostile code.

`--deny-untrusted-plugins` refuses every plugin in the run, including plugins
that do not declare `sandbox`. It is the recommended flag for CI. Without
either flag, a plugin that declares nothing keeps running with the current
user's permissions, which is the historical behaviour and is not a security
boundary.

The trust decision belongs to the core, not to the plugin: the manifest field
is enforced by the kernel, and the policy is set once per run and inherited by
every step.

### Egress filtering: proxy built, kernel enforcement still missing

`permissions.network` already carries structure — `host`, `port`, and an
explicit `any_host` escape hatch. That structure was never enforced. The
sandbox collapsed the whole list into "network was declared, so grant the
network", so a plugin stating `api.telegram.org:443` received unrestricted
egress, and the manifest read like a constraint that was in force.

A per-destination filter is not implementable inside either backend: `bwrap`
cannot express one (it either drops the network namespace or shares the host's),
and `sandbox-exec` only understands `allow`/`deny network*`. So the enforceable
case is now explicit:

- `any_host: true` — blanket egress, granted, and now something the author has to
  write on purpose.
- a list of concrete hosts without `any_host` — **not** silently widened. The
  sandbox denies the network, and under `network: allow` the run stops with
  `network_not_enforceable` and an explanation.

Official LLM plugins that previously declared a precise host were moved to
`any_host: true` with the intended target kept in `note`. That is not a
tightening — they already had blanket egress — it removes a false claim from the
manifests. The declared host lists in those notes are still aspirational.

On top of that, `internal/plugin/egress.go` implements the standard design used
by other agent sandboxes: the proxy runs in the **unsandboxed** WEDRA process on
loopback, the plugin receives `HTTP_PROXY`/`HTTPS_PROXY`, and the proxy allows
only declared destinations. It resolves every A/AAAA record, refuses private,
loopback, link-local and CGNAT ranges before dialling, then dials the **IP
literal** so DNS cannot rebind between the check and the connection. `CONNECT`
is restricted to TLS ports, and `stop` closes hijacked tunnels explicitly because
`http.Server.Shutdown` does not. If the proxy cannot start, the plugin does not
run (`sandbox_unavailable`).

**The proxy is currently dormant, and saying otherwise would be the easy mistake.**
A plugin inside a separate network namespace cannot reach the host: measured on
WSL2 with `bwrap` and `slirp4netns`, a service listening on the host's
`127.0.0.1` is refused from inside, and the stack's gateway address is
unreachable too. So the proxy's loopback address is simply not visible to a
sandboxed plugin, and `HTTP_PROXY` is not handed to it — pointing a cooperative
plugin at a dead address would only hand it a refusal on every request. The
allowlist code stays in the tree with its tests, but no current backend gives a
plugin a reachable host loopback, so nothing calls it. It is plumbing for a
future shared-netns backend, not a control in force today.

### The plugin's network namespace is now always its own

`--unshare-net` used to be added only when the plugin declared no network, which
meant a plugin declaring `any_host: true` ran with the **host's** network
namespace. That was a wider hole than unfiltered egress: the plugin could reach
every service bound to the host's loopback, including WEDRA's own HTTP API.

The namespace is now always separate, and egress is provided by a userspace
stack (`slirp4netns`): `tap0` inside the plugin's namespace, traffic NAT-ed by the
host. Measured inside such a sandbox:

| From inside the plugin | Result |
| --- | --- |
| host service on `127.0.0.1` | refused (`ConnectionRefusedError`) |
| the stack's gateway `10.0.2.2` | unreachable |
| internet by IP (`1.1.1.1:443`) | reachable |
| DNS via the stack's forwarder | resolves |

Two details the implementation depends on, both found the hard way:

- The plugin must not start before `tap0` exists, or its first requests fail.
  WEDRA runs the command behind a gate: `/bin/sh -c 'while [ ! -e "$1" ]; ...;
  exec "$@"'`, and the gate file is created only after the interface is up. The
  internal `$$` cannot be used to find the namespace either — under `--unshare-pid`
  it is a PID-namespace-relative pid, useless to `slirp4netns`. The host-visible
  pid comes from `/proc/<bwrap>/task/*/children`, and **its network namespace is
  checked before attaching**: aiming `slirp4netns` at the `bwrap` process itself
  adds `tap0` to the *host* namespace, which breaks the host.
- `/etc/resolv.conf` is a symlink on most hosts, and `bwrap` cannot create a file
  over a symlink (`Can't create file`). The override is bound at the *resolved*
  path. It is needed at all because the host's resolver address is not reachable
  from inside the isolated namespace, so without the override every DNS lookup
  times out.

**What this still does not buy.** Per-destination filtering. Inside
`unshare -Ur -n` — the same primitives `bwrap` uses — the sandbox is `UID=0` in a
user namespace that owns the network namespace, and on both a Debian 13.7 host
(kernel `6.12.107+deb13-amd64`) and WSL2 (kernel `6.18.33.2-microsoft-standard-WSL2`)
it can `ip link set lo up`, `nft add table`, `nft add chain` and `nft delete
table`. Rules installed inside the plugin's own namespace are not a control: the
plugin deletes them, or runs `nft flush ruleset`, before dialling. A userspace
stack does not change that — it hands the whole namespace outbound connectivity,
and the plugin dials through it directly rather than through a proxy.

So a plugin that declares `any_host: true` gets **unfiltered** egress, but no
longer inside the host's network namespace. That is the trade: the host's own
services are out of reach, the destination is not filtered.

Enforcing per-destination egress on Linux needs privilege on the *host*: a veth
pair with nftables applied in the host namespace (`CAP_NET_ADMIN` or a privileged
helper), or cgroup-scoped egress as systemd applies it. A user namespace cannot
reach the host's nftables, so host-side rules would hold — but they cannot be
installed unprivileged, and a setuid helper in a CLI people install without
reading is a poor trade.

This still does not address exfiltration through a file in the scratch
directory, through stdout, or through a DNS tunnel carried inside request names.

### The agent cannot simply omit the gate (MCP `run_pipeline`)

MCP is the only surface where the agent can initiate a run, so it is the only one
that enforces approval ordering. `run_pipeline` refuses, before any run exists,
when the **first** step whose plugin declares network, disk-write or secret
capabilities has no `core/human_gate` before it: `E_GATE_REQUIRED`, with the step
and plugin in the error payload. A gate placed *after* the dangerous step does not
count - the person would see the result, not the intent. Steps with no such
declarations run ungated, so ordinary local work (word counts, CSV parsing) is
not blocked by this rule.

What this control is and is not:

- It reads **declarations**. `permissions` are not an OS sandbox, so a plugin that
  lies in its manifest (`filesystem: none` while writing anyway) is not caught
  here. That is what `sandbox: untrusted` plus the OS isolator is for.
- It does not evaluate `when` or `foreach`. A step guarded by a false condition
  may never execute, but that is not knowable statically, so it counts as
  executing. An unneeded gate is the safe direction.
- It covers `run_pipeline` only. `exec_plugin` bypasses pipelines and gates by
  design, behind `--allow-agent-exec`, and that gap is known rather than closed
  (see `docs/mcp.md`). The HTTP API and the CLI start runs from a human surface
  and do not apply the rule.

The operator can turn the requirement off with `wedra mcp
--allow-unapproved-runs` for local trusted use. The bypass is not silent: the
server prints a warning to stderr at startup and appends a record per bypassed
run to `<runs-dir>/gate-bypass.jsonl`, flushed to disk before the run starts. If
the record cannot be written, the run is refused.

### Fixed: an unset `network` field is now "deny"

`network: deny` has always been the documented default, but the gate only ran
when the field was literally `deny`. A pipeline that omitted `network` matched
neither branch, so it got **no network check at all**, and the subprocess was
handed `WEDRA_NETWORK=allow` — the empty field granted network while the docs
promised it denied it.

An earlier attempt at this was reverted because it refused existing pipelines
whose plugins declare a host. That migration has now been done, so the fix is
in place. `pipeline.EffectiveNetwork` is the single place that resolves the
field, and the runner, both validators and the editor all go through it — the
bug existed because four layers each re-implemented `p.Network == "deny"` and
drifted apart.

Making the empty field mean `deny` surfaced two more consequences of the same
drift, both fixed here:

- The validators reported "valid" for a pipeline the runner then refused
  (`network: allow` plus a `host:port` list, which is not enforceable). There is
  now `E_NETWORK_NOT_ENFORCEABLE`, so `validate` and `run` agree.
- The GUI editor treated `allow` as the default and dropped it when serializing,
  because the empty field used to mean `allow`. Under the new meaning that
  silently turned a permission into a prohibition: a pipeline round-tripped
  through the editor stopped being able to reach the network. An explicit
  `network: allow` now survives the round-trip, while a genuinely unset field
  stays unset.

MCP keeps its own, stricter check (`checkPipelineSafety` refuses any network
declaration outright, whatever the pipeline asks for) — that is deliberate and
separate from the pipeline-level gate.

`TestNetworkPolicyMatrix` in `internal/execution` pins the resulting matrix,
including the two unset rows that used to be the known gap, and
`TestRunNetworkUnsetEnvIsDeny` in `internal/core` pins the value the subprocess
actually receives.
