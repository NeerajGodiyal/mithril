# MCP operator controls

Mithril MCP is read-only by default. The `operator` profile can start, stop, or
restart one fixed systemd service, but only after a separate Ed25519 approver
signs a short-lived challenge. The pre-dispatch phases must also be
acknowledged by a pinned mTLS audit receiver before systemd is invoked; later
results remain durable locally while remote delivery is retried.

Use three identities:

- **Node host:** Mithril, the MCP server, systemd, active and historical public
  keys, and the audit mTLS client.
- **Approver host:** the approval private key and `mithril mcp approve`.
- **Audit host:** `mithril-audit`, its private store, and its mTLS server key.

For a pilot, separate OS accounts may share hardware. Do not put the approver
private key or the only audit copy on the node host.

## Build

```bash
make build
```

This creates `mithril` and `mithril-audit`. The audit receiver requires a Unix
host with a private, local filesystem.

## Approval key

Create the key on the approver host in a mode-0700 directory:

```bash
./mithril mcp init-approval-key \
  /absolute/private/approver.seed \
  /absolute/private/approver.pub
```

Copy only `approver.pub` into the active public-key directory on both the node
and audit hosts, and retain the same key in each history directory. File names
do not define key IDs; Mithril derives the ID from the public key.

The public-key directories must be root-owned with mode 0750. Every `.pub`
file must be root-owned with mode 0440. Give the directory and files the group
of the service that reads them. Replace the example users and groups with the
accounts that run MCP over SSH and `mithril-audit`:

On the node host:

```bash
MCP_USER=mithril
MCP_GROUP=mithril
sudo install -d -o root -g "$MCP_GROUP" -m 0750 \
  /etc/mithril-mcp/approvers-active \
  /etc/mithril-mcp/approvers-history
sudo install -o root -g "$MCP_GROUP" -m 0440 approver.pub \
  /etc/mithril-mcp/approvers-active/primary.pub
sudo install -o root -g "$MCP_GROUP" -m 0440 approver.pub \
  /etc/mithril-mcp/approvers-history/primary.pub
sudo install -d -o "$MCP_USER" -g "$MCP_GROUP" -m 0700 \
  /var/lib/mithril-mcp/control
```

On the audit host:

```bash
AUDIT_USER=mithril-audit
AUDIT_GROUP=mithril-audit
sudo install -d -o root -g "$AUDIT_GROUP" -m 0750 \
  /etc/mithril-audit/approvers-active \
  /etc/mithril-audit/approvers-history
sudo install -o root -g "$AUDIT_GROUP" -m 0440 approver.pub \
  /etc/mithril-audit/approvers-active/primary.pub
sudo install -o root -g "$AUDIT_GROUP" -m 0440 approver.pub \
  /etc/mithril-audit/approvers-history/primary.pub
sudo install -d -o "$AUDIT_USER" -g "$AUDIT_GROUP" -m 0700 \
  /var/lib/mithril-audit
```

The writable state directories above must exist before either process starts.
They are owned by the process user and have mode 0700.

## Audit transport

Use a dedicated CA to issue:

- one server certificate for the audit host name;
- one client certificate for each node host.

Keep private keys mode 0600. Certificate and CA files must not be writable by
group or other users. Derive each certificate's lowercase SPKI pin with:

```bash
openssl x509 -in certificate.pem -pubkey -noout |
  openssl pkey -pubin -outform DER |
  openssl dgst -sha256 |
  awk '{print $2}'
```

The node's audit client file is strict JSON:

```json
{
  "version": 1,
  "endpoint": "https://audit.example:9443",
  "server_name": "audit.example",
  "server_spki_pin": "<64 lowercase hex characters>",
  "client_certificate_path": "/absolute/path/node-client.pem",
  "client_private_key_path": "/absolute/path/node-client.key",
  "server_ca_path": "/absolute/path/audit-ca.pem"
}
```

Start the receiver on the audit host:

```bash
./mithril-audit \
  --listen AUDIT_PRIVATE_IP:9443 \
  --store /var/lib/mithril-audit/control-audit.jsonl \
  --server-cert /absolute/private/audit-server.pem \
  --server-key /absolute/private/audit-server.key \
  --client-ca /absolute/private/client-ca.pem \
  --client-pin '<node client SPKI pin>' \
  --approver-keys-dir /etc/mithril-audit/approvers-active \
  --approver-history-keys-dir /etc/mithril-audit/approvers-history \
  --target-id node-mainnet-1 \
  --systemd-unit mithril.service \
  --systemd-scope system
```

`mithril-audit` is a long-running foreground process. Run this command as a
dedicated service under systemd or another supervisor, use
`Restart=on-failure`, and verify that the service is active before enabling
operator controls.

Restrict the listener with the host firewall. mTLS and SPKI pins are required;
the receiver has no shell, action, event-query, or administration endpoint.

## Start the MCP server

First ensure SSH can connect without a prompt:

```bash
ssh NODE true
```

Generate a client-neutral stdio entry:

```bash
./mithril mcp config \
  --ssh NODE \
  --remote-binary /absolute/path/mithril \
  --profile operator \
  --enable-control \
  --allow-action start \
  --allow-action stop \
  --allow-action restart \
  --approver-keys-dir /etc/mithril-mcp/approvers-active \
  --approver-history-keys-dir /etc/mithril-mcp/approvers-history \
  --control-target-id node-mainnet-1 \
  --systemd-unit mithril.service \
  --systemd-scope system \
  --control-state-dir /var/lib/mithril-mcp/control \
  --audit-client-config /etc/mithril-mcp/audit-client.json
```

Put the resulting command and argument array into any stdio-capable MCP
client. The client launches `mithril mcp`; do not run it separately in another
terminal.

## Action flow

1. Call `mithril_service_status`.
2. Call `mithril_prepare_service_action`.
3. On the approver host, run `mithril mcp approve` and paste the challenge at
   the hidden prompt.
4. Return the complete approval bundle to
   `mithril_execute_service_action`.
5. Call `mithril_verify_service_action` only when the result says verification
   is still needed.

A completed systemd job does not prove that the node is ready. For start and
restart, inspect `node_readiness`; if it is incomplete, call
`mithril_diagnose`.

If the phase is `outcome_unknown`, never repeat or retry that action. Wait
until `new_action_allowed_at`, read fresh service status, then prepare and
approve a new action only if it is still intended.

## Key rotation

Retain every public key referenced by the audit chain.

1. Copy the current active keys into the history directory on both hosts.
2. Add the new public key to both active directories.
3. Restart the audit receiver first, then the MCP server, and test the new key.
4. Remove the old key from the audit receiver's active directory and restart
   it.
5. Remove the old key from the node's active directory and restart MCP.

Never remove the old key from either history directory. Historical keys can
finish and restore an existing action, but cannot authorize a new action.

## Recovery

Before restoring a replacement node, revoke or fence the old node's audit
client identity so two hosts cannot append as the same target. Create a fresh
mode-0700 control-state directory, copy `control-audit.jsonl` from the audit
host through the administrator recovery channel, then run:

```bash
./mithril mcp restore-control \
  --control-state-dir /absolute/private/new-control-state \
  --approver-keys-dir /absolute/private/approvers-active \
  --approver-history-keys-dir /absolute/private/approvers-history \
  --audit-client-config /absolute/private/audit-client.json \
  --control-target-id node-mainnet-1 \
  --systemd-unit mithril.service \
  --systemd-scope system
```

Restore fails unless the copied chain is valid and exactly matches the live,
pinned receiver summary. It never overwrites different state or calls
systemctl.

If an append reports uncertain durability after a short write, the receiver
truncates only the incomplete record back to its last verified prefix, fsyncs,
and then stops. Restart it only after checking disk health and comparing that
prefix with the audit host.

The current store is bounded to 64 MiB and 65,536 records. Alert on capacity
well before either limit. Reaching the limit fails closed; authenticated
rollover is not yet implemented. On Linux, monitor both:

```bash
stat -c %s /var/lib/mithril-audit/control-audit.jsonl
wc -l < /var/lib/mithril-audit/control-audit.jsonl
```

For example, alert by 48 MiB or 49,152 records so there is time to disable
operator controls and preserve the chain before capacity is exhausted.
