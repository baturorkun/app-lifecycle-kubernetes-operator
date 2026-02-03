# rke2-pre-shutdown.service ✅

Short guide for installing and testing the `rke2-pre-shutdown.service` systemd unit used to run a pre-shutdown hook that updates `NamespaceLifecyclePolicy` CRs before a node shuts down.

---

## Purpose 🔧
- Run a short, idempotent script that informs the operator (via Kubernetes API) that this RKE2 node is shutting down.
- Ensures `NamespaceLifecyclePolicy` or other cleanup actions can be applied gracefully before the node goes offline.

> Important: The hook must complete quickly — the service includes a 30s start timeout by default (`TimeoutStartSec=30`). Make your script resilient to transient API failures.

---

## Files
- Systemd unit: `linux/rke2-before-shutdown-service/rke2-pre-shutdown.service` (example content below)
- Hook script (example location): `/usr/local/bin/rke2-pre-shutdown.sh` — this must exist and be executable.

Example unit (from this repo):

```ini
[Unit]
Description=Pre-shutdown hook to update NamespaceLifecyclePolicy CRs
DefaultDependencies=no

# Run BEFORE the system shuts down
Before=shutdown.target reboot.target halt.target

# Ensure Kubernetes API is still available
After=network-online.target rke2-server.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/rke2-pre-shutdown.sh

# Do not let the system hang forever
TimeoutStartSec=30

# systemd waits for this to exit
RemainAfterExit=yes

[Install]
WantedBy=shutdown.target reboot.target halt.target
```

---

## Prerequisites ✅
- The hook script (`/usr/local/bin/rke2-pre-shutdown.sh`) is present and executable:

```bash
sudo install -m 0755 linux/rke2-before-shutdown-service/rke2-pre-shutdown.sh /usr/local/bin/rke2-pre-shutdown.sh
# or
sudo cp linux/rke2-before-shutdown-service/rke2-pre-shutdown.sh /usr/local/bin/
sudo chmod 0755 /usr/local/bin/rke2-pre-shutdown.sh
```

- If the script talks to the Kubernetes API, ensure it has access to a valid kubeconfig (typically via node's service account, or by locating `/etc/rancher/rke2/rke2.yaml` if applicable).

- Systemd (systemd-based Linux distribution) on the RKE2 node.

---

## Installation ✨
1. Copy the unit file to `/etc/systemd/system/`:

```bash
sudo cp linux/rke2-before-shutdown-service/rke2-pre-shutdown.service /etc/systemd/system/rke2-pre-shutdown.service
```

2. Reload systemd units:

```bash
sudo systemctl daemon-reload
```

3. Enable the unit so it is wanted during shutdown/reboot:

```bash
sudo systemctl enable rke2-pre-shutdown.service
```

> Enabling makes systemd include the unit in the shutdown sequence (it creates symlinks under the appropriate targets).

---

## Testing 🧪
- To simulate and test the hook without rebooting the node, run it directly:

```bash
sudo systemctl start rke2-pre-shutdown.service
sudo systemctl status -l rke2-pre-shutdown.service
sudo journalctl -u rke2-pre-shutdown.service --no-pager
```

- For a real test, perform a controlled reboot and watch logs on both the node and in cluster (operator/controller logs) to verify the pre-shutdown action executed correctly.

---

## Troubleshooting ⚠️
- If the hook doesn't run during shutdown, confirm:
  - The unit is enabled: `systemctl is-enabled rke2-pre-shutdown.service`
  - There are no typos in the unit installed at `/etc/systemd/system/`
  - The hook script is executable and exits successfully within 30s

- Check logs:

```bash
sudo journalctl -u rke2-pre-shutdown.service -b -u
```

- If the script needs Kubernetes API access but fails during shutdown, consider:
  - Using local kubeconfig path (`/etc/rancher/rke2/rke2.yaml`) and proper RBAC
  - Making the script retry briefly but exit well before the TimeoutStartSec runs out

---

## Uninstall / Disable ❌

```bash
sudo systemctl disable rke2-pre-shutdown.service
sudo rm /etc/systemd/system/rke2-pre-shutdown.service
sudo systemctl daemon-reload
```

---

## Notes & Best Practices 💡
- Keep the hook short and idempotent.
- Log to stdout/stderr (captured by journalctl) for easy debugging.
- Prefer marking the node cordoned/draining earlier using normal Kubernetes mechanisms when possible — the pre-shutdown hook is a last-mile step to ensure policy updates occur.

---

If you want, I can add a sample `rke2-pre-shutdown.sh` script tailored to this operator's expected behavior (update CRs and ensure safe replica scaling). ⚙️
