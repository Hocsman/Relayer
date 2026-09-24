# Observability & Monitoring with Prometheus & Grafana

Relayer includes a built-in, zero-dependency telemetry engine that collects real-time supervision metrics, human arbitration latency, and security guardrail violations.

This guide explains how to spin up the official Prometheus and Grafana stack in 1 command, connect it to your Relayer instance, and explore the pre-provisioned dashboard.

---

## 1. Quickstart with Docker Compose

Relayer provides a preconfigured Docker Compose stack with:
- **Prometheus** (auto-scraping Relayer metrics every 5s)
- **Grafana** (pre-provisioned with the official `Relayer — Autonomous AI Agent Supervision & Security` dashboard)

### Step 1: Enable Prometheus Export in Relayer

In your `config.yaml` (or via CLI `--config`), enable the Prometheus endpoint:

```yaml
version: 1
backend: auto

telemetry:
  enabled: true
  service_name: "relayer-local"
  prometheus:
    enabled: true
    address: ":9090"
    path: "/metrics"
```

Start Relayer (CLI, Desktop GUI or `relayer serve`):
```bash
./relayer
```

The metrics are computed from the audit journal's entries as they are written,
so they need `audit` enabled and mean the same thing whichever front end runs.
Before v0.8.9 the web gateway journaled no detection and none of the policy's
decisions: its runs reported no pending prompt, an empty decision-duration
histogram and only people's decisions, and an agent that exited on its own
stayed counted in `relayer_sessions_active`.

Verify that the metrics endpoint is responding locally:
```bash
curl http://localhost:9090/metrics
```

### Step 2: Start Prometheus & Grafana

Run from the root of the Relayer repository:

```bash
docker compose -f docker-compose.telemetry.yml up -d
```

Containers started:
- **Prometheus**: Accessible at `http://localhost:9090`
- **Grafana**: Accessible at `http://localhost:3000`

### Step 3: View the Live Dashboard

Open **[http://localhost:3000](http://localhost:3000)** in your browser.
- **Zero-login needed**: Anonymous Viewer access is enabled by default.
- To make modifications, log in with username `admin` and password `admin`.
- The dashboard is automatically loaded as the home dashboard.

To shut down the monitoring stack:
```bash
docker compose -f docker-compose.telemetry.yml down
```

---

## 2. Standalone Grafana Setup (Manual Import)

If your team already operates a shared Grafana instance:

1. Copy [`docs/grafana-dashboard.json`](file:///c:/Users/Duc_Monster/Projets/Relayer/docs/grafana-dashboard.json) (or from `telemetry/grafana/dashboards/relayer-dashboard.json`).
2. In Grafana, navigate to **Dashboards** $\rightarrow$ **New** $\rightarrow$ **Import**.
3. Upload the JSON file or paste its contents.
4. Select your Prometheus datasource and click **Import**.

---

## 3. Metrics Reference & PromQL Queries

The dashboard visualizes the following standard Prometheus metrics exported by Relayer:

| Metric Name | Type | Labels | Description | PromQL in Dashboard |
|---|---|---|---|---|
| `relayer_sessions_active` | Gauge | `agent_id` | Supervised agent sessions currently running | `sum(relayer_sessions_active{agent_id=~"$agent_id"})` |
| `relayer_events_pending` | Gauge | — | Prompts detected and not yet decided, by the operator or the policy, nor withdrawn | `relayer_events_pending` |
| `relayer_decisions_total` | Counter | `adapter`, `agent_id`, `decision`, `decision_by`, `outcome`, `rule` | Decisions taken, one per decision entry in the journal, counted when journaled, before delivery: `decision` is `allow`, `deny` or `ask` (a typed answer), `decision_by` is `human` or `policy`, and `rule` the policy rule that decided, `default` when none did, a person's decision included | `sum by (action) (relayer_decisions_total{agent_id=~"$agent_id"})` |
| `relayer_guardrail_violations_total` | Counter | `reason`, `rule` | Security guardrail violations blocked | `sum by (rule_id) (relayer_guardrail_violations_total)` |
| `relayer_decision_duration_seconds` | Histogram | `adapter`, `agent_id`, `decision_by`, `le` | Time from a prompt's detection to its decision, by the operator or the policy (`decision_by`) | `histogram_quantile(0.95, sum(rate(relayer_decision_duration_seconds_bucket[5m])) by (le))` |
| `relayer_events_detected_total` | Counter | `adapter`, `agent_id`, `event_type`, `risk`, `sensitive` | Prompts and process exits detected; `sensitive` marks the confidential ones | `sum(rate(relayer_events_detected_total[1m])) * 60` |
| `relayer_operator_inputs_total` | Counter | `agent_id` | Manual lines sent to agent terminals | `sum(rate(relayer_operator_inputs_total[1m])) * 60` |
| `relayer_control_events_total` | Counter | `agent_id`, `action`, `outcome` | Terminal hand-overs between operators: `requested`, `granted`, `declined`, `released`, `forced`, `attached`, `detached` | `sum by (outcome) (increase(relayer_control_events_total{action="forced"}[1h]))` |
| `relayer_recording_events_total` | Counter | `agent_id`, `action`, `outcome` | Session recording lifecycle: `started`, `finished`, `exported`, `deleted` | `sum(increase(relayer_recording_events_total{outcome="failed"}[1h]))` |

The last two are not on the bundled dashboard; their queries are suggestions. A
forced takeover and a recording that failed to open are the two signals most
worth an alert: the first is somebody seizing a colleague's terminal, the second
is a session that was meant to be recorded and was not.

Two panels of the bundled dashboard group by labels their metric does not
carry, `action` and `rule_id`, and so show a single unlabelled series. Group
decisions by `decision` and `decision_by`, and guardrail violations by `reason`,
to split them.

Since v0.8.9, `relayer_decisions_total` counts each decision once. It used to
count every policy evaluation as well: an automatic decision twice, a prompt a
person answered once as the policy's `ask` and once as theirs, and a prompt
still waiting as decided. Totals, and the dashboard's "Total Decisions",
decisions-by-action and decisions-per-minute panels, read about half what they
did for automatic decisions, and no longer count prompts nobody has answered.
The Metrics panel of the Desktop GUI and the web interface reads the same
counter, and no longer shows each prompt the policy handed to a person as an
automatic deny. The policy's decisions are counted under the rule that made
them, where they used to fall under `default`.

---

## 4. Security & Privacy Guarantee

All metrics exported by Relayer follow the strict **Zero-Leakage Model**:
- **No secrets or tokens**: API keys, passwords, bearer tokens, or sensitive parameters are never included in metric labels or values.
- **Bounded cardinality**: Metric labels (`agent_id`, `adapter`, `decision`, `decision_by`, `outcome`, `rule`, `reason`, `risk`, `action`) have finite, safe cardinalities preventing memory bloat in time-series databases.
- **Encrypted & Authenticated**: For multi-tenant setups, the OTLP exporter supports custom authentication headers (`Authorization: Bearer ...`) over TLS (`https://`).
