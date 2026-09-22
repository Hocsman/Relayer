# Web gateway

`relayer serve` runs the supervisor without a desktop application and exposes it
over HTTP and WebSocket. It serves the same interface the Desktop GUI uses, so
agents running on a cloud devbox, an EC2 or GCP instance, or inside a container
can be supervised from a browser.

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
sent (for nginx, `proxy_set_header Host $host;`). A proxy that rewrites `Host` to
the upstream address makes every browser request look cross-origin, and the
gateway refuses it with `403`.

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

Binding to `127.0.0.1` with no token configured allows anonymous local access as
`local-operator`. Binding anywhere else with no token configured makes the
gateway generate one operator and one viewer secret and print both.

A tokenless gateway answers only to requests addressed to `127.0.0.1:PORT`,
`localhost:PORT` or `[::1]:PORT`, on every path, and refuses any other `Host`
with `403`. This is what stops DNS rebinding: a web page whose own domain
resolves to `127.0.0.1` reaches the loopback socket and controls its `Origin`,
but its browser still names the page's domain in `Host`.

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

The full trust boundary is described in
[security-model.md](security-model.md#web-gateway-and-remote-operators).
