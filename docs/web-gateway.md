# Web gateway

`relayer serve` runs the supervisor without a desktop application and exposes it
over HTTP and WebSocket. It serves the same interface the Desktop GUI uses, so
agents running on a cloud devbox, an EC2 or GCP instance, or inside a container
can be supervised from a browser. In a container, start it with an init process
(`docker run --init`, or tini): without one, nothing reaps an agent's orphaned
children, their zombies keep its process group alive, and every Stop of that
agent is reported as unconfirmed.

```bash
relayer serve --bind 0.0.0.0 --port 8080 \
  --token alice:s3cret,bob:hunter2 \
  --viewer-token review:look-only
```

> [!WARNING]
> The gateway serves plain HTTP. Binding anywhere other than `127.0.0.1`
> requires an external TLS terminator and network restriction. Treat a gateway
> URL, token included, as a credential equivalent to shell access on the
> supervising host.

## Origin and Host checks

A browser request is accepted only from the gateway's own origin: its `Origin`
header, when present, must name exactly the host and port the request was sent
to. A page served from another origin cannot call the API or open the
WebSocket, and that includes a page on **another port of the same machine** —
a dev server or a dashboard running locally is not trusted because it is local.
Clients that send no `Origin`, such as `curl` or a script, are unaffected.

Behind a reverse proxy, the proxy must **preserve the `Host` header** the browser
sent, port included (for nginx, `proxy_set_header Host $http_host;`: `$host`
drops the port, and the browser's `Origin` keeps it). A proxy that rewrites
`Host` to the upstream address makes every browser request look cross-origin,
and the gateway refuses it with `403`. Behind any proxy or tunnel, run the
gateway with `--token`: see [No token](#no-token) for what a tokenless one
accepts, which includes such a proxy's requests that carry no `Origin`.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--bind` | `127.0.0.1` | Interface to listen on. |
| `--port` | `8080` | Port to listen on. `0` picks a free one. |
| `--token` | — | Operator (read-write) token or tokens. |
| `--operator-token` | — | Alias for `--token`; both may be given and are merged. |
| `--viewer-token` | — | Viewer (read-only) token or tokens. |
| `--config` | platform default | Configuration file to load. |
| `--static-dir` | — | Serve the interface from a directory instead of the embedded bundle. |

On start the gateway prints the URLs, each with its token in the query string.

## Roles

A connection's role is resolved from the token it presents and is fixed for the
life of that connection.

| | `operator` | `viewer` |
| --- | --- | --- |
| Watch terminals, see prompts, read the audit trail | yes | yes |
| List and replay [recordings](recording.md) | yes | yes |
| See who else is connected | yes | yes |
| Answer prompts, send a line | yes | no |
| Take a terminal and type into it | yes | no |
| Stop, start, restart an agent | yes | no |
| Change settings, stop the run | yes | no |
| Export or delete a recording | yes | no |

Authorization is enforced server-side in the RPC dispatcher and on inbound
terminal frames, not in the interface. A viewer who replays a mutating call by
hand, or hand-crafts a binary terminal frame, is rejected. The read-only
affordances in the interface reflect the server's decision; they do not make it.

A viewer's window resize is accepted and ignored rather than refused, so a
passive observer never perturbs the terminal geometry of the operator working.

## Named tokens

A token may carry an identity:

```
--token alice:s3cret,bob:hunter2
--viewer-token review:look-only,auditor:read-me
```

The part before the colon is the identity recorded in the audit trail for every
decision, line, attachment, and terminal transfer that token performs. Without a
colon the identity is just `operator` or `viewer`.

An identity is only as meaningful as the secret's distribution. Relayer
authenticates a token, never a person: two people sharing `alice:s3cret` are
indistinguishable in the journal.

Tokens are matched in constant time and accepted from either a `token` query
parameter or an `Authorization: Bearer` header.

## No token

Binding to `127.0.0.1` or `localhost` with no token configured allows anonymous
local access as `local-operator`. Binding anywhere else with no token configured makes the
gateway generate one operator and one viewer secret and print both.

A tokenless gateway answers only to requests addressed to `127.0.0.1:PORT`,
`localhost:PORT` or `[::1]:PORT`, on every path, and refuses any other `Host`
with `403`. This is what stops DNS rebinding: a web page whose own domain
resolves to `127.0.0.1` reaches the loopback socket and controls its `Origin`,
but its browser still names the page's domain in `Host`.

The same check refuses a tokenless gateway reached **through a forward or a
proxy that sends another name or port in `Host`**: an SSH tunnel to another
local port (`ssh -L 9000:127.0.0.1:8080`), an editor forward that picks a
different port, or a reverse proxy that preserves the public `Host` all get
`403`.

Two common set-ups are **accepted anonymously as `local-operator`** instead:

- a forward that keeps `127.0.0.1` or `localhost` **and the same port**
  (`ssh -L 8080:127.0.0.1:8080`, or an editor forward that reuses the remote
  port), which is indistinguishable from a local browser: every local process
  on the machine that opened the forward can operate the remote agents;
- a reverse proxy that **rewrites `Host` to the gateway's own address**, which
  is nginx's default (`proxy_set_header Host $proxy_host` for
  `proxy_pass http://127.0.0.1:8080`): every request through it that carries no
  `Origin`, such as `curl` or a script, is anonymous operator access for anyone
  who can reach the proxy.

Pass `--token` for any forward, tunnel or proxy: a gateway with tokens checks
`Origin` against `Host`, but does not pin `Host` itself.

Anonymous access is still access for **anything running locally as a client**:
another user on a shared machine, or a local program, can connect as
`local-operator` without a secret. On a machine you do not have to yourself,
pass `--token` even on loopback.

## Multiple operators

Several operators and viewers can watch the same session at once, with one
holding the terminal at a time. See [sharing.md](sharing.md).

## Recording

Sessions can be recorded to asciicast files and replayed in the interface. See
[recording.md](recording.md).

## Security boundaries

The gateway is an authentication boundary, not a sandbox. A holder of an
operator token has the same authority over the supervised agents as somebody
sitting at the local Desktop GUI, including interactive attachment, which
bypasses prompt detection, policy, and arbitration by design.

An operator also receives each agent's full command line, because it can edit
and restart the agents: a credential passed as an argument is visible to every
operator token. A viewer receives only each agent's executable name, and no host
path: not the configuration's, the audit journal's, the recordings' or an
agent's working directory, and not in an error message either.

A viewer does receive every agent's terminal, as it is: the output snapshots
carry the screen verbatim, to viewers as to operators. What an agent prints —
a token it echoes, a file it displays, a secret in a command it runs — reaches
every viewer. Only the prompt cards are redacted: a prompt's summary, its
tool-call badge and every notification carry bounded, redacted text, and a
prompt whose text must not be shown carries none. A viewer token is therefore
a token to read the terminals, and should be given only to people who may.

The full trust boundary is described in
[security-model.md](security-model.md#web-gateway-and-remote-operators).
