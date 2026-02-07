import os
import time
import json
import subprocess
import sys
import distro
import argparse

# Import Proton Client
try:
    from proton.api import Session
    from proton.exceptions import ProtonError
except ImportError:
    print("Error: Failed to import proton.api. Is proton-python-client installed?")
    sys.exit(1)

# Configuration
TARGET_CITIES = os.environ.get("TARGET_CITIES", "San Jose").split(",")
TARGET_COUNTRY = os.environ.get("TARGET_COUNTRY", "")
ENV_FILE = "/project/.env"
GLUETUN_CONTAINER = "gluetun"
CHECK_INTERVAL = int(os.environ.get("CHECK_INTERVAL", "300"))
HEALTH_CHECK_INTERVAL = int(os.environ.get("HEALTH_CHECK_INTERVAL", "60"))
LOAD_CHECK_INTERVAL = int(os.environ.get("LOAD_CHECK_INTERVAL", "900"))
PING_TARGET = "8.8.8.8"
SESSION_FILE = os.environ.get("SESSION_FILE", "/data/proton_session.json")
LOG_DIR = os.environ.get("LOG_DIR", "/tmp/proton_sidecar/logs")
CACHE_DIR = os.environ.get("CACHE_DIR", "/tmp/proton_sidecar/cache")

# Credentials
PROTON_USERNAME = os.environ.get("PROTON_USERNAME")
PROTON_PASSWORD = os.environ.get("PROTON_PASSWORD")


def log(msg):
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}", flush=True)


class ProtonManager:
    def __init__(self):
        self.session = None
        self.ensure_dirs()
        self.ensure_session()

    def ensure_dirs(self):
        os.makedirs(LOG_DIR, exist_ok=True)
        os.makedirs(CACHE_DIR, exist_ok=True)

        # Ensure session directory exists and is secure (chmod 700)
        session_dir = os.path.dirname(SESSION_FILE)
        if not os.path.exists(session_dir):
            os.makedirs(session_dir, exist_ok=True)

        try:
            # Secure the directory so only the owner (root in container) can read it
            # This propagates to the host folder (proton-session)
            os.chmod(session_dir, 0o700)
        except Exception as e:
            log(f"Warning: Failed to secure session directory permissions: {e}")

    def ensure_session(self):
        # 1. Try to load existing session
        if os.path.exists(SESSION_FILE):
            try:
                log("Loading session from disk...")
                with open(SESSION_FILE, "r") as f:
                    dump = json.load(f)

                self.session = Session.load(
                    dump,
                    log_dir_path=LOG_DIR,
                    cache_dir_path=CACHE_DIR,
                    tls_pinning=False,
                )
                self.session.enable_alternative_routing = True
                log("Session loaded successfully.")
                return
            except Exception as e:
                log(f"Failed to load session: {e}. Starting fresh.")

        # 2. Authenticate fresh
        self.authenticate()

    def authenticate(self):
        if not PROTON_USERNAME or not PROTON_PASSWORD:
            log("Error: PROTON_USERNAME and PROTON_PASSWORD must be set.")
            sys.exit(1)

        log(f"Authenticating as {PROTON_USERNAME}...")
        self.session = Session(
            api_url="https://api.protonmail.ch",
            appversion="Other",
            user_agent="Python/3.11",
            log_dir_path=LOG_DIR,
            cache_dir_path=CACHE_DIR,
            tls_pinning=False,
        )
        self.session.enable_alternative_routing = True

        try:
            self.session.authenticate(PROTON_USERNAME, PROTON_PASSWORD)
            log("Authentication successful.")
            self.save_session()
        except ProtonError as e:
            log(f"Authentication failed: {e}")
            sys.exit(1)

    def save_session(self):
        try:
            dump = self.session.dump()
            with open(SESSION_FILE, "w") as f:
                json.dump(dump, f)
            log("Session saved to disk.")
        except Exception as e:
            log(f"Failed to save session: {e}")

    def get_servers(self):
        """
        Fetches servers using the official client, handling 401 refreshes automatically.
        """
        endpoint = "/vpn/logicals"

        try:
            # We attempt the request. The client might throw an error.
            # However, the official client usage docs say we should catch 401 and refresh manually.
            # proton_session.api_request(endpoint="custom_api_endpoint")

            # The client's api_request returns the JSON response directly (usually).
            response = self.session.api_request(endpoint=endpoint, method="get")
            return response.get("LogicalServers", [])

        except ProtonError as e:
            if e.code == 401:
                log("Token expired (401). Refreshing session...")
                try:
                    self.session.refresh()
                    self.save_session()
                    log("Session refreshed. Retrying request...")
                    # Retry once
                    response = self.session.api_request(endpoint=endpoint, method="get")
                    return response.get("LogicalServers", [])
                except Exception as refresh_err:
                    log(f"Failed to refresh session: {refresh_err}")
                    # Force re-auth next time or crash
                    self.authenticate()
                    return []
            elif e.code == 503:
                log("Proton API unavailable (503).")
            else:
                log(f"Proton API Error {e.code}: {e}")
            return []
        except Exception as ex:
            log(f"Unexpected error fetching servers: {ex}")
            return []


def check_connectivity():
    cmd = [
        "docker",
        "exec",
        "-it",
        GLUETUN_CONTAINER,
        "ping",
        "-c",
        "3",
        "-W",
        "2",
        PING_TARGET,
    ]
    try:
        subprocess.check_call(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        return True
    except:
        return False


def get_current_server_from_env():
    if not os.path.exists(ENV_FILE):
        return ""
    try:
        with open(ENV_FILE, "r") as f:
            for line in f:
                if line.strip().startswith("PROTON_SERVER_NAME="):
                    return line.strip().split("=", 1)[1]
    except:
        pass
    return ""


def update_env(server_info):
    server_name = server_info["Name"]
    # Extract IP and PublicKey from the first server entry (WireGuard)
    # The 'Servers' list typically contains multiple entries, but for logicals they usually share the same physical server details
    # We look for the one with X25519PublicKey (WireGuard key)

    wg_server = None
    for s in server_info.get("Servers", []):
        if "X25519PublicKey" in s:
            wg_server = s
            break

    if not wg_server:
        log(f"Error: No WireGuard (X25519) key found for server {server_name}")
        return False

    server_ip = wg_server["EntryIP"]
    public_key = wg_server["X25519PublicKey"]

    log(f"Updating ENV: Name={server_name}, IP={server_ip}, Key={public_key[:10]}...")

    try:
        lines = []
        if os.path.exists(ENV_FILE):
            with open(ENV_FILE, "r") as f:
                lines = f.readlines()

        new_lines = []
        # Variables we manage
        managed_vars = {
            "PROTON_SERVER_NAME": server_name,
            "WIREGUARD_ENDPOINT_IP": server_ip,
            "WIREGUARD_ENDPOINT_PORT": "51820",  # Standard WireGuard port
            "WIREGUARD_PUBLIC_KEY": public_key,
        }

        # Track which vars we've found/updated
        found_vars = {k: False for k in managed_vars}

        for line in lines:
            updated = False
            for key, val in managed_vars.items():
                if line.strip().startswith(f"{key}="):
                    new_lines.append(f"{key}={val}\n")
                    found_vars[key] = True
                    updated = True
                    break
            if not updated:
                new_lines.append(line)

        # Append missing vars
        if new_lines and not new_lines[-1].endswith("\n"):
            new_lines.append("\n")

        for key, val in managed_vars.items():
            if not found_vars[key]:
                new_lines.append(f"{key}={val}\n")

        with open(ENV_FILE, "w") as f:
            f.writelines(new_lines)
        return True
    except Exception as e:
        log(f"Error updating env: {e}")
        return False


def restart_gluetun():
    log("Recreating Gluetun (to apply new ENV)...")
    # We use docker-compose to force recreate the container, which pulls the new ENV variables.
    # We assume the project uses a standard docker-compose.yml or is controlled via Compose.

    # Default command using standalone binary (installed in Dockerfile)
    cmd = ["docker-compose", "up", "-d", "--force-recreate", GLUETUN_CONTAINER]

    # Check if we are in the project directory and specific file exists
    if os.path.exists("/project/docker-compose.yml"):
        cmd = [
            "docker-compose",
            "-f",
            "/project/docker-compose.yml",
            "up",
            "-d",
            "--force-recreate",
            GLUETUN_CONTAINER,
        ]

    try:
        subprocess.run(cmd, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except subprocess.CalledProcessError as e:
        log(f"Failed to recreate gluetun: {e}")
        log(f"Stdout: {e.stdout.decode().strip() if e.stdout else 'None'}")
        log(f"Stderr: {e.stderr.decode().strip() if e.stderr else 'None'}")

        # Fallback to restart if compose fails (better than nothing, though env won't update)
        log("Fallback: Restarting container directly...")
        subprocess.run(["docker", "restart", GLUETUN_CONTAINER])


def main():
    parser = argparse.ArgumentParser(description="ProtonVPN Sidecar Manager")
    parser.add_argument(
        "--check-only", action="store_true", help="Fetch best server and exit"
    )
    parser.add_argument(
        "--list-cities", action="store_true", help="List all available cities and exit"
    )
    parser.add_argument(
        "--country",
        type=str,
        help="Filter by country code (e.g. US) when listing cities",
    )
    args = parser.parse_args()

    log("VPN Manager (Official Client) Started")
    # Don't sleep if we are just listing or checking
    if not args.check_only and not args.list_cities:
        time.sleep(5)

    manager = ProtonManager()

    if args.list_cities:
        log("Fetching full server list...")
        servers = manager.get_servers()
        if not servers:
            log("No servers retrieved.")
            sys.exit(1)

        # Aggregate data: (Country, City) -> {count, load_sum}
        city_stats = {}

        for s in servers:
            # Filter inactive or maintenance
            if s.get("Status") != 1:
                continue

            country = s.get("EntryCountry", "??")
            city = s.get("City", "Unknown")

            if args.country and args.country.upper() != country:
                continue

            key = (country, city)
            if key not in city_stats:
                city_stats[key] = {"count": 0, "load_sum": 0}

            city_stats[key]["count"] += 1
            city_stats[key]["load_sum"] += s.get("Load", 0)

        print(f"\n{'-' * 60}")
        print(f"{'COUNTRY':<8} {'CITY':<30} {'SERVERS':<10} {'AVG LOAD':<10}")
        print(f"{'-' * 60}")

        for (country, city), stats in sorted(city_stats.items()):
            avg_load = stats["load_sum"] // stats["count"] if stats["count"] > 0 else 0
            print(f"{country:<8} {city:<30} {stats['count']:<10} {avg_load}%")
        print(f"{'-' * 60}\n")
        return

    if args.check_only:
        log("Running in CHECK ONLY mode...")
        servers = manager.get_servers()
        if not servers:
            log("No servers retrieved.")
            sys.exit(1)

        current_server_name = get_current_server_from_env()
        # We don't check connectivity in check-only mode usually, unless requested.
        # But we can print what we find.

        current_load = 100
        best_server = None
        best_load = 100

        candidates = []
        for s in servers:
            if s.get("Name") == current_server_name:
                current_load = s.get("Load", 100)

            if s.get("City") in TARGET_CITIES or any(
                c.lower() == s.get("City", "").lower() for c in TARGET_CITIES
            ):
                if s.get("Status") == 1:
                    candidates.append(s)

        if candidates:
            candidates.sort(key=lambda x: x.get("Load", 100))
            best_server = candidates[0]
            best_load = best_server.get("Load", 100)

        print(f"\n--- REPORT ---")
        print(
            f"Current Server (Env): {current_server_name or 'None'} (Load: {current_load}%)"
        )
        if best_server:
            print(
                f"Best Server in {TARGET_CITIES}: {best_server['Name']} (Load: {best_load}%)"
            )
            print(f"Entry IP: {best_server.get('EntryIP')}")
        else:
            print(f"No active servers found in {TARGET_CITIES}")
        print("--------------")
        return

    while True:
        try:
            last_health_check = 0
            last_load_check = 0

            # Inner loop to handle timing
            while True:
                now = time.time()

                # Check 1: Health (Frequent)
                if now - last_health_check >= HEALTH_CHECK_INTERVAL:
                    last_health_check = now
                    is_healthy = check_connectivity()

                    if not is_healthy:
                        log(
                            "Unhealthy connection detected! Initiating immediate failover..."
                        )
                        # Force load check logic to run immediately to find a new server
                        last_load_check = 0
                    else:
                        # If healthy, we only proceed to load check if it's time
                        if now - last_load_check < LOAD_CHECK_INTERVAL:
                            time.sleep(5)
                            continue

                # Check 2: Load (Infrequent, or triggered by bad health)
                if now - last_load_check >= LOAD_CHECK_INTERVAL:
                    last_load_check = now

                    servers = manager.get_servers()
                    current_server_name = get_current_server_from_env()

                    # Re-check health just in case we are here due to load check interval
                    is_healthy = check_connectivity()

                    current_load = 100
                    best_server = None
                    best_load = 100
                    candidates = []

                    if servers:
                        for s in servers:
                            # Match Current
                            if s.get("Name") == current_server_name:
                                current_load = s.get("Load", 100)

                            # Filter by Country if set
                            if (
                                TARGET_COUNTRY
                                and s.get("EntryCountry") != TARGET_COUNTRY
                            ):
                                continue

                            # Match Candidates
                            if s.get("City") in TARGET_CITIES or any(
                                c.lower() == s.get("City", "").lower()
                                for c in TARGET_CITIES
                            ):
                                if s.get("Status") == 1:
                                    candidates.append(s)

                    if candidates:
                        candidates.sort(key=lambda x: x.get("Load", 100))
                        best_server = candidates[0]
                        best_load = best_server.get("Load", 100)

                    log_msg = f"Health: {'OK' if is_healthy else 'BAD'} | Current: {current_server_name} ({current_load}%)"

                    if best_server:
                        log_msg += f" | Best: {best_server['Name']} ({best_load}%)"
                    log(log_msg)

                    # Decision Logic
                    should_switch = False
                    target = None
                    reason = ""

                    if not is_healthy:
                        should_switch = True
                        reason = "Unhealthy Connection"
                        if best_server:
                            target = best_server["Name"]

                    elif best_server and current_server_name:
                        if current_load > (best_load + 20):
                            should_switch = True
                            target = best_server["Name"]
                            reason = f"Load Optimization ({current_load}% > {best_load}% + 20%)"

                    if should_switch and target and target == current_server_name:
                        should_switch = False

                    if should_switch and target:
                        log(f"Initiating switch to {target}. Reason: {reason}")
                        # Pass the full server object (best_server) instead of just the name
                        if update_env(best_server):
                            restart_gluetun()
                            # Wait for restart to settle before resuming checks
                            time.sleep(45)
                            # Reset timers to give it time to stabilize
                            last_health_check = time.time()
                            last_load_check = time.time()

                time.sleep(5)

        except Exception as e:
            log(f"Main loop error: {e}")
            time.sleep(30)


if __name__ == "__main__":
    main()
