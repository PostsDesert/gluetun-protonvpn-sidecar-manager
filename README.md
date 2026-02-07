# ProtonVPN Sidecar Manager for Gluetun & Tailscale

This project provides a self-healing, load-balancing VPN gateway using:
1.  **Gluetun**: Connects to ProtonVPN (WireGuard).
2.  **Tailscale/Headscale**: Acts as an exit node, routing traffic through the VPN.
3.  **Sidecar Manager**: A custom Go agent that monitors server load/health, automatically switches to the best Proton server, and updates the connection without killing the Tailscale tunnel.

## Architecture

To ensure zero-downtime for the Tailscale node during VPN server switches, this stack uses the **"Network Anchor"** pattern:

*   **`network-anchor`**: A tiny container that holds the network namespace and ports open.
*   **`gluetun`**: Joins the anchor's network. It can be restarted freely to change servers.
*   **`tailscale-vpn`**: Joins the anchor's network. It stays running even if Gluetun restarts.
*   **`vpn-manager`**: Monitors the connection. If the server is overloaded or down, it fetches a new configuration from the Proton API, updates the `.env` file, and recreates the `gluetun` container.
*   **`configurator`**: A tiny ephemeral container that applies critical routing rules, firewall NAT, and performance tuning (MSS Clamping) on every startup. This ensures the Tailscale -> Gluetun routing works correctly in high-performance Kernel mode.

## Prerequisites

1.  **ProtonVPN Account**: You need your **Main Account Credentials** (username/password used for the website) for the API manager, AND your **WireGuard Private Key** for the actual connection.
2.  **Headscale/Tailscale**: You need a control server URL (if using Headscale) and a **Reusable Pre-Auth Key**.

## Setup Instructions

### 1. Directory Setup
Copy the example compose file to your project root (parent directory):
```bash
cp docker-compose.example.yml ../docker-compose.yml
```

### 2. Configuration (.env)
Create a `.env` file in the project root with the following variables:

```env
# --- Proton Manager Config ---
PROTON_USERNAME=your_proton_username
PROTON_PASSWORD=your_proton_password

# Target Cities (Comma separated)
TARGET_CITIES=San Jose,New York
# Optional: Restrict to country code (e.g. US)
TARGET_COUNTRY=US

# Monitoring Intervals (Seconds)
HEALTH_CHECK_INTERVAL=60
LOAD_CHECK_INTERVAL=900

# --- WireGuard Static Config ---
# Get these from ProtonVPN Dashboard -> Downloads -> WireGuard Configuration
WIREGUARD_PRIVATE_KEY=your_private_key_here
WIREGUARD_ADDRESSES=10.2.0.2/32

# --- Tailscale Config ---
TS_AUTHKEY=your_reusable_auth_key
TS_LOGIN_SERVER=https://headscale.example.com

# --- Dynamic Variables (Managed by Sidecar - Leave Empty) ---
PROTON_SERVER_NAME=
WIREGUARD_ENDPOINT_IP=
WIREGUARD_ENDPOINT_PORT=
WIREGUARD_PUBLIC_KEY=
```

### 3. Build and Run
```bash
# Build the manager image
docker compose build vpn-manager

# Start the stack
docker compose up -d
```

### 4. Verify
*   **Manager Logs**: `docker compose logs -f vpn-manager`
*   **Connectivity**: `docker exec -it tailscale-proton-exit nslookup google.com`

## Utilities

### List Available Cities
To see which cities are available and their current load:
```bash
docker compose run --rm vpn-manager ./manager --list-cities
# Or filter by country
docker compose run --rm vpn-manager ./manager --list-cities --country US
```

### Manual Server Switch
If you want to force a switch immediately, you can restart the manager container, as it checks logic on startup:
```bash
docker compose restart vpn-manager
```

## Performance Optimization (Advanced)

For users seeking maximum throughput (especially on high-speed connections or hybrid CPUs), consider the following optimizations.

### 1. CPU Pinning (Hybrid Architectures)
If you are running on an Intel 12th Gen+ (Alder Lake) or similar hybrid CPU with Performance (P) and Efficiency (E) cores, the VPN containers may be scheduled on slow E-cores, significantly degrading speed.

To fix this, edit your `docker-compose.yml` and add the `cpuset` directive to both `gluetun` and `tailscale-vpn` services, pinning them to your P-Cores (e.g., 0-11):

```yaml
  gluetun:
    # ...
    cpuset: "0-11"  # Replace with your P-Core IDs
```

```

### 2. Network Tuning (Applied Automatically)
The stack includes a `configurator` service that automatically applies:
*   **MSS Clamping**: Prevents TCP fragmentation issues common in double-VPN setups.
*   **Routing Fixes**: Ensures correct routing between Tailscale and ProtonVPN interfaces.
*   **NAT Masquerading**: Enables internet access for connected clients.
*   **UDP Buffer Tuning**: Increases `rmem`/`wmem` to 2.5MB to handle bursty WireGuard traffic.
