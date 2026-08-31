# Mithril MCP

Mithril MCP works with Codex, Claude Code, Cursor, VS Code, and other clients
that support stdio MCP. It lets them inspect a Mithril node through its RPC,
metrics, logs, state, and replay data. The default `monitor` profile is
read-only.

The client launches Mithril as a local stdio process and stops it when the
session ends. A remote connection uses the same stdio transport over SSH.

## Quick start

Build Mithril:

```bash
make build
```

### Codex

```bash
./mithril mcp setup codex
codex mcp list
```

The setup command records the binary's absolute path, so Codex can launch it
from any directory.

### Claude Code

```bash
./mithril mcp setup claude
claude mcp get mithril
```

The Codex and Claude Code setup commands register Mithril for the current user
and record the binary's absolute path.

### Cursor

Run `./mithril mcp config`, then put its output under `mcpServers.mithril` in
`~/.cursor/mcp.json` or a project `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "mithril": {
      "type": "stdio",
      "command": "/absolute/path/to/mithril",
      "args": ["mcp"]
    }
  }
}
```

### VS Code

```bash
./mithril mcp setup vscode
```

For a workspace-specific setup, run `./mithril mcp config`, then put its output
under `servers.mithril` in `.vscode/mcp.json`:

```json
{
  "servers": {
    "mithril": {
      "type": "stdio",
      "command": "/absolute/path/to/mithril",
      "args": ["mcp"]
    }
  }
}
```

### Other MCP clients

`mithril mcp config` prints a standard stdio server entry:

```json
{
  "type": "stdio",
  "command": "/absolute/path/to/mithril",
  "args": ["mcp"]
}
```

Add the entry as a server named `mithril` using your client's MCP settings.
MCP clients use different names for the outer configuration object, but the
server entry is the same.

## Remote node

Use a dedicated SSH key and a dedicated account that can read only the node
configuration and local observability endpoints needed by the selected MCP
profile. Give the account no general SSH command access: keep a valid login
shell because `sshd` uses it to launch forced commands, but place the key in
that account's `authorized_keys` with a forced Mithril command and OpenSSH's
`restrict` option. For example:

```text
restrict,command="/absolute/path/to/mithril mcp --config /absolute/path/to/config.toml --profile monitor" ssh-ed25519 AAAA... mithril-mcp
```

The forced command ignores any command requested by the client. `restrict`
disables PTY allocation, port, agent, and X11 forwarding, and `~/.ssh/rc`.
Keep `PermitUserEnvironment no` for this account. This restriction applies to
this key: remove other authorized keys and disable password and
keyboard-interactive authentication for the account, or enforce the same
restrictions account-wide with `Match User`, `ForceCommand`,
`DisableForwarding yes`, `PermitTTY no`, `PermitTunnel no`, and
`PermitUserRC no`. Do not reuse an administrator key or grant this account
write access to node state, validator keys, wallets, or signer sockets.

Pin the server host key in a separate known-hosts file after verifying its
fingerprint through an independent channel. A minimal client alias is:

```sshconfig
Host mithril-mcp
  HostName NODE_ADDRESS
  User mithril-mcp
  IdentityFile ~/.ssh/mithril_mcp_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile ~/.ssh/known_hosts_mithril_mcp
  StrictHostKeyChecking yes
```

Then confirm that SSH works without a password or host-key prompt. The forced
MCP command expects protocol input, so use the generated client configuration
for the actual connection rather than testing it with an arbitrary remote
command.

```bash
ssh -T -o BatchMode=yes mithril-mcp </dev/null
```

Codex can add the remote server directly:

```bash
./mithril mcp setup codex \
  --ssh mithril-mcp \
  --remote-binary /absolute/path/to/mithril
```

For another client, generate its stdio entry:

```bash
./mithril mcp config \
  --ssh mithril-mcp \
  --remote-binary /absolute/path/to/mithril
```

SSH uses the pinned host alias, dedicated identity, and authentication
configuration. When `authorized_keys` forces the same Mithril command, the
requested remote command generated here is deliberately ignored by `sshd`.

## Check the connection

Ask the client to call `mithril_mcp_info`, then `mithril_diagnose`. Both calls
should return structured node information. The remaining monitor tools cover
slots, metrics, logs, replay state, rewards, bank hashes, and shutdown state.

## Agent instructions

Mithril sends usage guidance during MCP initialization, so a project rule is
not required. To pin the same workflow in a repository, add this to
`AGENTS.md`, `CLAUDE.md`, or your client's project instructions:

> For Mithril node questions, use the Mithril MCP tools. Call
> `mithril_mcp_info` first, then `mithril_diagnose`; treat `unknown` or
> `evidence_complete=false` as incomplete evidence.

## Profiles

- `monitor` is the default read-only profile.
- `diagnostic` adds account reads, transaction simulation, and profiling. Use
  `./mithril mcp config --profile diagnostic` only when those tools are needed.
  A remote profile argument is ignored when `authorized_keys` forces the
  `monitor` command; remote diagnostic access needs a separately reviewed
  forced command and dedicated key or account alias.

Access is controlled by the local user account or the SSH connection.
