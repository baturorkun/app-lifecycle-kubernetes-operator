# Metrics Server for Kubernetes Node Usage Monitoring

A simple Python HTTP server that provides CPU and memory usage percentages as JSON. Designed to run as a systemd service on each Kubernetes worker node for alternative metrics collection (e.g., RKE2 environments where kubelet stats API may not be available).

## Features

- Returns CPU and memory usage percentages as JSON
- Health check endpoint
- Runs as a systemd service with automatic restart
- Listens on all interfaces (0.0.0.0) for node IP access

## Endpoints

- `GET /metrics` - Returns CPU and memory usage percentages
- `GET /` - Same as /metrics (root endpoint)
- `GET /health` - Returns health status

## Installation on Ubuntu/Debian

### 1. Install Python 3 and pip (if not already installed)

```bash
sudo apt update
sudo apt install -y python3 python3-pip
```

### 2. Install psutil package

```bash
sudo pip3 install psutil
```

### 3. Create service directory and copy files

```bash
sudo mkdir -p /opt/metrics-server
sudo cp metrics.py /opt/metrics-server/
sudo chmod +x /opt/metrics-server/metrics.py
```

### 4. Install systemd service

```bash
sudo cp metrics-server.service /etc/systemd/system/
sudo systemctl daemon-reload
```

### 5. Configure port (optional)

By default, the service runs on port 9090. To change the port, edit the service file:

```bash
sudo nano /etc/systemd/system/metrics-server.service
```

Change the port in the `ExecStart` line:
```
ExecStart=/usr/bin/python3 /opt/metrics-server/metrics.py 9090
```

Replace `9090` with your desired port number.

Alternatively, you can use an environment variable (uncomment the `Environment` line in the service file and modify the script to read it).

### 6. Start and enable the service

```bash
sudo systemctl enable metrics-server.service
sudo systemctl start metrics-server.service
```

### 7. Verify the service is running

```bash
sudo systemctl status metrics-server.service
```

### 8. Test the endpoints

```bash
# Test metrics endpoint
curl http://localhost:9090/metrics

# Test health endpoint
curl http://localhost:9090/health

# Test from node IP (replace with your node's IP)
curl http://<node-ip>:9090/metrics
```

## Service Management

### Start the service
```bash
sudo systemctl start metrics-server
```

### Stop the service
```bash
sudo systemctl stop metrics-server
```

### Restart the service
```bash
sudo systemctl restart metrics-server
```

### View service logs
```bash
sudo journalctl -u metrics-server.service -f
```

### Check service status
```bash
sudo systemctl status metrics-server.service
```

## Configuration

### Change Port

Edit `/etc/systemd/system/metrics-server.service` and modify the port in the `ExecStart` line:

```
ExecStart=/usr/bin/python3 /opt/metrics-server/metrics.py <PORT>
```

Then reload and restart:
```bash
sudo systemctl daemon-reload
sudo systemctl restart metrics-server
```

### Change User

By default, the service runs as `nobody:nogroup` for security. To change the user, edit the service file:

```
User=your-user
Group=your-group
```

## Kubernetes Integration

Once the service is running on each worker node, configure your `NamespaceLifecyclePolicy` to use it:

```yaml
spec:
  adaptiveThrottling:
    signalChecks:
      checkNodeUsage:
        enabled: true
        cpuThresholdPercent: 40
        memoryThresholdPercent: 80
        slowdownPercent: 60
        scrape:
          source: ":9090/metrics"  # Port and path (no IP - operator will use each node's IP)
          cpu: "cpu_usages.percentage"
          mem: "memory_usages.percentage"
```

The operator will automatically send requests to `http://{node-ip}:9090/metrics` for each worker node.

## Troubleshooting

### Service fails to start

Check the logs:
```bash
sudo journalctl -u metrics-server.service -n 50
```

Common issues:
- **Port already in use**: Change the port in the service file
- **Permission denied**: Ensure the service directory has correct permissions
- **psutil not found**: Reinstall psutil: `sudo pip3 install psutil`

### Cannot access from other nodes

- Ensure the service is listening on `0.0.0.0` (default)
- Check firewall rules: `sudo ufw allow 9090/tcp`
- Verify the service is running: `sudo systemctl status metrics-server`

### Service keeps restarting

Check logs for errors:
```bash
sudo journalctl -u metrics-server.service -f
```

## Security Notes

- The service runs as `nobody:nogroup` user (non-root)
- Systemd security settings are enabled (NoNewPrivileges, PrivateTmp, ProtectSystem)
- Only the service directory is writable
- Consider adding firewall rules to restrict access to Kubernetes nodes only

## Uninstallation

```bash
sudo systemctl stop metrics-server
sudo systemctl disable metrics-server
sudo rm /etc/systemd/system/metrics-server.service
sudo systemctl daemon-reload
sudo rm -rf /opt/metrics-server
```
