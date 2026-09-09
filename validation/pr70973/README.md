# Resource Control Grafana verification

PR: https://github.com/pingcap/tidb/pull/70973

Dashboard and TiDB revision: `4d9dfae5c361fbdf89262c6fdb81991330882bcf`.

Both local deployments used TiDB binaries built from that revision with Go 1.26.7, using `make server` for Classic and `make server NEXT_GEN=1` for TiDB X. Classic used a Community TiKV (`39c62c85bb71e580fb34e89858b5f2e70f2f890e`) and Classic PD. TiDB X used NextGen PD, Cloud Storage Engine TiKV (`100d0dce768a69413f202fe87b1b312d04f7bf66`), MinIO, a SYSTEM TiDB, and two business TiDB instances in `keyspace1`. These are local, single-storage-node validation environments, not production topologies.

Grafana 10.4.19 and Prometheus 3.5.0 were configured with 5-second scrapes. The PR dashboards were imported with only deployment settings changed (datasource, dashboard title/UID, default variables and display timezone/time range); every target expression matches the PR JSON exactly. All eight scrape targets were healthy. The screenshots are unedited browser captures, with the graph scrolled slightly to include its time axis. Grafana's warning icon denotes the existing Graph panel's Angular deprecation, not a failed query.

The same 225-second workload ran on both architectures in resource group `dashboard_demo` (`RU_PER_SEC=100000 BURSTABLE`): each cycle updated an 8 KiB value and read it back. Four connections per instance generated requested rates of 15, 200, 0, 65, 250, 15 and 0 cycles/s on instance 1, with phase durations of 35, 35, 25, 35, 35, 35 and 25 seconds. Instance 2 used half those rates. Each architecture completed 28,628 cycles, with zero SQL errors. Recorded SQL status confirmed the PR commit on all five TiDB processes.

Both dashboards showed the two peaks and idle valleys. Client RU showed two distinct instance series; the instance selector was exercised in Grafana on both architectures. Queries also verified instance/resource-group filtering and nonempty, finite RU/RRU/WRU series. Client RU maxima were approximately 2,746/1,376 RU/s on Classic and 3,017/1,513 RU/s on TiDB X; each returned to zero during idle. These are observability checks, not a performance comparison between architectures.

All captures show 2026-09-09 12:56:32–13:00:37 UTC, with `resource_group=dashboard_demo` and all business TiDB instances selected.

## Classic

![Classic RU](classic-ru.png)

![Classic Client RU](classic-client-ru.png)

## TiDB X

![TiDB X RU](tidbx-ru.png)

![TiDB X Client RU](tidbx-client-ru.png)

Production scrape intervals/query costs, multiple business keyspaces, and AP workloads were not tested. Refund and reset edge cases were covered by the earlier Prometheus synthetic query tests, not by this SQL workload.
