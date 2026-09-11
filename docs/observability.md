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

Start Relayer (CLI or Desktop GUI):
```bash
./relayer
```

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
| `relayer_events_pending` | Gauge | — | Prompts waiting for operator decision | `relayer_events_pending` |
| `relayer_decisions_total` | Counter | `agent_id`, `action` | Cumulative decisions applied (`allow`, `deny`, `auto`, `custom`) | `sum by (action) (relayer_decisions_total{agent_id=~"$agent_id"})` |
| `relayer_guardrail_violations_total` | Counter | `agent_id`, `rule_id` | Security guardrail violations blocked | `sum by (rule_id) (relayer_guardrail_violations_total)` |
| `relayer_decision_duration_seconds` | Histogram | `agent_id`, `action`, `le` | Human arbitration reaction time | `histogram_quantile(0.95, sum(rate(relayer_decision_duration_seconds_bucket[5m])) by (le))` |
| `relayer_events_detected_total` | Counter | `agent_id`, `adapter`, `event_type`, `risk` | Cumulative sensitive events intercepted | `sum(rate(relayer_events_detected_total[1m])) * 60` |
| `relayer_operator_inputs_total` | Counter | `agent_id` | Manual lines sent to agent terminals | `sum(rate(relayer_operator_inputs_total[1m])) * 60` |

---

## 4. Security & Privacy Guarantee

All metrics exported by Relayer follow the strict **Zero-Leakage Model**:
- **No secrets or tokens**: API keys, passwords, bearer tokens, or sensitive parameters are never included in metric labels or values.
- **Bounded cardinality**: Metric labels (`agent_id`, `action`, `rule_id`, `risk`, `adapter`) have finite, safe cardinalities preventing memory bloat in time-series databases.
- **Encrypted & Authenticated**: For multi-tenant setups, the OTLP exporter supports custom authentication headers (`Authorization: Bearer ...`) over TLS (`https://`).
