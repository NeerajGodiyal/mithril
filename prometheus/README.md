# Mithril deterministic monitoring

This directory contains the model-independent monitoring layer:
Prometheus scrapes the node and off-host services, rules decide when evidence
is unsafe, and Alertmanager keeps the alert record while delivery is retried.

## Files

| File | Purpose |
|---|---|
| `prometheus.yml` | Scrape discovery, rule loading, and Alertmanager discovery |
| `rules/mithril.yml` | 51 alert rules across node, monitor, host, and delivery signals |
| `tests/mithril_alerts_test.yml` | A direct firing test for every alert, plus critical recovery cases |
| `inventory.example.json` | Credential-free deployment inventory to copy before signing |
| `blackbox.yml` | Bounded Mithril JSON-RPC and TCP probes |
| `alertmanager.yml` | mTLS Telegram and deadman routes, with no SES dependency |
| `alertmanager-with-ses.yml` | The same routes plus SES delivery for critical alerts |

## Runtime files

Repository files contain no deployment endpoint, token, recipient, or provider
credential. Deployment supplies root-owned `file_sd` JSON under
`/etc/prometheus/targets/`:

```text
alertmanager.json
blackbox-exporter.json
mithril-agent.json
mithril-monitor.json
mithril-node.json
mithril-notifier.json
mithril-rpc-offhost.json
node-exporter.json
prometheus-peer.json
```

Every target must carry the same bounded `deployment_id`. It must exactly match
both `deployment_id` in the protected monitor configuration and `manifest_id`
in the signed inventory. The monitor exports that signed identity separately,
and a missing, duplicate, malformed, or mismatched identity is critical.
The monitor scrape preserves exporter labels so target discovery cannot shadow
the signed identity fields. RPC probe targets also carry a nonsecret bounded
`probe_name`. A blackbox target must not contain a credential: its URL is
visible in the Prometheus targets API
and to blackbox_exporter even though it is not copied into a metric label.
`mithril-rpc-offhost.json` must target the numeric IP configured by
`rpc.bind_address`; the RPC server rejects DNS aliases. The same-port loopback
companion is available only to processes on the node.
Authenticated reference-provider checks belong in `mithril-monitor`, whose
root-owned configuration is not exported as a target label. The blackbox probe
calls Mithril's supported `getEpochInfo` method and requires a numeric
`absoluteSlot`; it does not assume the node implements Solana `getHealth`.
Configuration rejects duplicate reference hostnames, but the deployment must
still choose two separately operated providers; URLs cannot prove that
organizational independence.

The monitor's `node_rpc_url` and `node_exporter_metrics_url` must use the same
numeric private host. Ports and paths may differ. This prevents a slot from one
node being combined with filesystem evidence from another. DNS names and
different public/private front doors are deliberately refused at this evidence
boundary.

A minimal protected `providers.toml` has this shape; provider credentials and
deployment paths remain runtime values:

```toml
deployment_id = "replace-at-deploy"
systemd_unit = "mithril.service"
systemd_scope = "system"
node_rpc_url = "http://<same-private-IP>:8899"
node_exporter_metrics_url = "http://<same-private-IP>:9100/metrics"
reference_primary_url = "https://primary.example/rpc"
reference_fallback_url = "https://fallback.example/rpc"
inventory_manifest_path = "/etc/mithril-monitor/inventory.json"
inventory_signature_path = "/etc/mithril-monitor/inventory.sig"
inventory_public_key_path = "/etc/mithril-monitor/inventory.pem"
```

The local Prometheus self target also carries two required boolean policy
labels. Set `monitoring_peer_required=true` when a second monitoring instance
is deployed, otherwise set it to `false` and configure the independent
deadman. Set `notification_fallback_required=true` when SES is required by the
deployment policy. Missing or non-boolean policy labels alert.

The node publishes `mithril_monitoring_schema_ready=0` before its metrics
endpoint becomes available and changes it to `1` only after the fixed runtime,
replay, verification, and finality families are initialized. Missing-family
alerts are held while this value is `0`; malformed values and divergence remain
alertable. A missing, duplicate, non-boolean, or six-hour-old zero readiness
value is itself a critical alert. This metric describes schema initialization,
not node health.

Run node_exporter with:

```text
--collector.systemd
--collector.systemd.enable-restarts-metrics
```

The required collector set is `systemd`, `filesystem`, `meminfo`, `cpu`, and
`pressure`. Set `systemd_unit` in the protected monitor configuration to the
exact service being observed. `systemd_scope` must be `system`: node_exporter's
systemd collector observes the system manager, not per-user managers. MCP may
still control a user unit in a local or development setup, but this production
monitoring profile refuses to claim service-state coverage for it.

Protected configuration and TLS-key ownership checks rely on Unix filesystem
semantics. Native Windows startup fails closed; use WSL2 or a Linux virtual
machine instead of weakening those checks.

## Signed deployment inventory

`mithril-monitor` verifies a detached Ed25519 signature over the raw inventory
bytes before it publishes any target or filesystem inventory. The schema is
fixed to eight target jobs and three filesystem roles. Every target is
required except `mithril-agent`, which may remain optional until that process
is deployed. The `root`, `accounts`, and `ledger` filesystem roles are always
required.

When the separate agent application is deployed, install its rule file as
`/etc/prometheus/rules/mithril-agent.yml` and its loopback scrape target as
`/etc/prometheus/targets/mithril-agent.json`. The existing Alertmanager route
then sends its deterministic alerts through the same Telegram notifier; the
agent never receives the bot credential. The target file is:

```json
[
  {
    "targets": ["127.0.0.1:9191"],
    "labels": {"deployment_id": "replace-at-deploy"}
  }
]
```

Use the same deployment ID as the signed inventory and every other target.
Validate the rule and Prometheus configuration, reload Prometheus, and confirm
the `mithril-agent` target is up before granting execution authority.

Copy `inventory.example.json`, set `manifest_id` to the same nonsecret
deployment identifier used by the protected monitor and every Prometheus
target, change the deployment mountpoints, then sign the final bytes on an
offline workstation. OpenSSL 3 can generate and verify the artifacts as
follows:

```bash
umask 077
openssl genpkey -algorithm ED25519 -out inventory-private.pem
openssl pkey -in inventory-private.pem -pubout -out inventory.pem
openssl pkeyutl -sign -rawin -inkey inventory-private.pem \
  -in inventory.json -out inventory.sig
openssl pkeyutl -verify -rawin -pubin -inkey inventory.pem \
  -in inventory.json -sigfile inventory.sig
```

Keep `inventory-private.pem` off the operations host. Install only the exact
signed `inventory.json`, the raw 64-byte `inventory.sig`, and `inventory.pem`
as protected regular files readable by `mithril-monitor`. Configure their
clean absolute paths in `providers.toml`. The monitor verifies the files on
every collection and pins the signed manifest identifier and digest for its
lifetime, so a deliberate inventory change requires replacing all matching
artifacts and restarting the monitor.

The monitor reads raw `node_filesystem_*` values only from the configured
private `node_exporter_metrics_url`. It publishes bounded role metrics instead
of mountpoints:

```text
mithril_monitor_identity_info{signed_deployment_id,systemd_unit,systemd_scope}
mithril_expected_target{target_job,required}
mithril_expected_filesystem_role{role,required}
mithril_filesystem_avail_bytes{role}
mithril_filesystem_size_bytes{role}
```

Rules require the exact signed target and role inventories, detect a required
target with no discovered scrape target, reject missing, duplicate,
non-integer, non-finite, or oversized filesystem evidence, and alert when a
required role has less than 10% available space. If the monitor itself is
absent, the Prometheus self target remains the independent anchor for the
missing-monitor alert.

## Alert delivery

Use `alertmanager.yml` when Telegram is the only delivery route and set
`notification_fallback_required=false`. Use `alertmanager-with-ses.yml` when
critical alerts must also go to SES, set the policy label to `true`, and
configure the notifier's SES canary. The no-SES file never references an SES
password or attempts email delivery.

Alertmanager connects to `mithril-notifier` with a client certificate issued
by a dedicated, single-purpose private CA. That CA must issue certificates only
to Alertmanager notifier clients; the notifier trusts that client CA.
Alertmanager separately trusts the CA that issued the notifier's server
certificate. The runtime host supplies both CA certificates, the client
certificate and key, and the notifier server certificate and key. The SES
configuration additionally needs the SMTP password, sender, and recipient.
Critical and warning routes send resolved events. Informational updates do not,
so a completed action cannot later look like an incident recovery.

The deadman alert always fires and includes `deployment_id`. Its receiver must
be replaced with an independent heartbeat service that can detect silence from
the primary monitoring host. A phone that only receives messages cannot do
that.

Notification probes run hourly by default. The rules require each probe to
succeed at least once every two hours, so a deployment must not configure a
probe interval above one hour. Probe health and freshness apply only to routes
marked configured by `mithril_notification_route_configured`. Real notifier
delivery counters describe Telegram webhook alerts only; synthetic Telegram
and SES canaries have separate attempt and failure counters. The SES probe
proves SMTP acceptance only; it does not prove inbox delivery. Alertmanager's
own failure counter separately covers errors in the configured email route.
If the deployment requires SES but the notifier reports it unconfigured, a
separate fallback-policy alert fires.

## Validation

```bash
promtool check config prometheus/prometheus.yml
promtool check rules prometheus/rules/mithril.yml
promtool test rules prometheus/tests/mithril_alerts_test.yml
amtool check-config prometheus/alertmanager.yml
amtool check-config prometheus/alertmanager-with-ses.yml
```

Missing `file_sd` warnings are expected on a development machine without the
runtime target files.

`promtool` and `amtool` accept the shipped templates as syntactically valid,
including the `replace-at-deploy` values they still contain. Check those
separately against the deployed copies, before starting the notifier:

```bash
mithril-notifier -verify-deploy-config /etc/alertmanager
mithril-notifier -verify-deploy-config /etc/prometheus
```

Run this as an `ExecStartPre=` so a deployment cannot start with placeholders
in place. This matters most for the deadman receiver: its shipped URL is
deliberately unroutable, so leaving it unreplaced makes Alertmanager retry it
every minute indefinitely, and the resulting delivery-failure counter keeps
`MithrilAlertDeliveryPipelineFailing` firing continuously. Refusing to start is
the quieter and more honest outcome. Either point the receiver at a real
off-host heartbeat, or remove the deadman route and its rule together — an
unroutable placeholder is worse than no deadman at all.

## Deployment boundary

These files provide the deterministic monitoring and alerting layer, not host
provisioning. Deployment still supplies protected service units, TLS material,
provider configuration, signed inventory artifacts, and an independent
deadman receiver.
