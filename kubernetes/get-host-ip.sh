#!/usr/bin/env bash

# Script: kubernetes/get-host-ip.sh
# Purpose: Find the host machine's IP address from within a Kind container
#          Useful when Kind runs inside Podman/Docker and host.containers.internal doesn't work

set -e

echo "Finding host IP address from Kind container..."
echo ""

# Method 1: Check default gateway (most common in Podman/Docker)
DEFAULT_GATEWAY=$(ip route | grep default | awk '{print $3}' | head -1)

if [[ -n "$DEFAULT_GATEWAY" ]]; then
    echo "Method 1 (Default Gateway): $DEFAULT_GATEWAY"
    
    # Test if gateway is reachable
    if ping -c 1 -W 1 "$DEFAULT_GATEWAY" > /dev/null 2>&1; then
        echo "✅ Gateway is reachable"
        echo ""
        echo "Suggested host IP: $DEFAULT_GATEWAY"
        echo ""
        echo "Test with:"
        echo "  curl http://$DEFAULT_GATEWAY:9090/metrics"
        exit 0
    else
        echo "⚠️  Gateway is not reachable"
    fi
fi

# Method 2: Check host.docker.internal (Docker Desktop)
if getent hosts host.docker.internal > /dev/null 2>&1; then
    HOST_DOCKER_IP=$(getent hosts host.docker.internal | awk '{print $1}' | head -1)
    echo "Method 2 (host.docker.internal): $HOST_DOCKER_IP"
    
    if ping -c 1 -W 1 "$HOST_DOCKER_IP" > /dev/null 2>&1; then
        echo "✅ host.docker.internal is reachable"
        echo ""
        echo "Suggested host IP: $HOST_DOCKER_IP"
        echo ""
        echo "Test with:"
        echo "  curl http://$HOST_DOCKER_IP:9090/metrics"
        exit 0
    fi
fi

# Method 3: Check host.containers.internal (Podman)
if getent hosts host.containers.internal > /dev/null 2>&1; then
    HOST_CONTAINERS_IP=$(getent hosts host.containers.internal | awk '{print $1}' | head -1)
    echo "Method 3 (host.containers.internal): $HOST_CONTAINERS_IP"
    
    # Note: In Podman+Kind setup, this might point to Kind container, not host
    echo "⚠️  Note: In Podman+Kind setups, this may point to Kind container, not host"
fi

# Method 4: Check bridge network gateway
BRIDGE_GW=$(ip route | grep -E '^172\.|^192\.168\.|^10\.' | awk '{print $1}' | cut -d'/' -f1 | head -1)
if [[ -n "$BRIDGE_GW" ]]; then
    echo "Method 4 (Bridge network): $BRIDGE_GW"
fi

echo ""
echo "⚠️  Could not automatically determine host IP"
echo ""
echo "Manual options:"
echo "1. If you know your host IP (e.g., 192.168.127.2), use it directly:"
echo "   curl http://192.168.127.2:9090/metrics"
echo ""
echo "2. Find host IP from host machine:"
echo "   # On macOS/Linux:"
echo "   ip addr show | grep 'inet ' | grep -v '127.0.0.1'"
echo "   # Or:"
echo "   hostname -I"
echo ""
echo "3. Check Kind container network:"
echo "   docker/podman inspect <kind-container> | grep Gateway"
echo ""
exit 1
