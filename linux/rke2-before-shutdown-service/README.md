# rke2-pre-shutdown.service

Systemd unit that runs a pre-shutdown hook before RKE2 shuts down on a node.

The hook sets `spec.action: Freeze` on all `NamespaceLifecyclePolicy` CRs and then waits until the operator has driven all of them to `status.phase: Frozen` before allowing the shutdown to proceed.

---

## How it works

The service uses the `ExecStop` + `RemainAfterExit` pattern:

1. At boot, `ExecStart=/bin/true` marks the service as **active** immediately.
2. During shutdown, systemd calls `ExecStop` (the actual script) before stopping `rke2-server.service`, because the unit is declared `After=rke2-server.service` (systemd reverses this for stop ordering).
3. The script patches all CRs to `action=Freeze` and polls `status.phase` until all reach `Frozen` or the timeout expires.
4. Once the script exits, systemd continues stopping `rke2-server.service` and then proceeds with shutdown.

`TimeoutStopSec=600` gives the operator up to 10 minutes to complete all freeze operations.

---

## Files

| File | Destination |
|---|---|
| `rke2-pre-shutdown.service` | `/etc/systemd/system/rke2-pre-shutdown.service` |
| `rke2-pre-shutdown.sh` | `/usr/local/bin/rke2-pre-shutdown.sh` |

---

## Installation

```bash
# Copy and make script executable
sudo install -m 0755 linux/rke2-before-shutdown-service/rke2-pre-shutdown.sh \
  /usr/local/bin/rke2-pre-shutdown.sh

# Copy unit file
sudo cp linux/rke2-before-shutdown-service/rke2-pre-shutdown.service \
  /etc/systemd/system/rke2-pre-shutdown.service

# Reload and enable
sudo systemctl daemon-reload
sudo systemctl enable rke2-pre-shutdown.service
```

---

## Testing without rebooting

```bash
# Simulate the ExecStop (runs the freeze script directly)
sudo systemctl stop rke2-pre-shutdown.service

# Check logs
sudo journalctl -u rke2-pre-shutdown.service --no-pager
cat /var/log/rke2-pre-shutdown.log

# Re-arm the service for the next shutdown test
sudo systemctl start rke2-pre-shutdown.service
```

---

## Troubleshooting

- **Service not running during shutdown**: confirm it is enabled (`systemctl is-enabled rke2-pre-shutdown.service`) and that the unit file is correctly installed.
- **Script fails to reach API**: verify `/etc/rancher/rke2/rke2.yaml` exists and the node's RBAC allows `get`/`patch` on `namespacelifecyclepolicies`.
- **Shutdown too slow**: reduce `TimeoutStopSec` in the unit file or reduce workload counts managed by the operator.

---

## Uninstall

```bash
sudo systemctl disable rke2-pre-shutdown.service
sudo rm /etc/systemd/system/rke2-pre-shutdown.service
sudo rm /usr/local/bin/rke2-pre-shutdown.sh
sudo systemctl daemon-reload
```
