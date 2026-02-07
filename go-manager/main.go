package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-proton-api"
)

// Constants
const (
	defaultCheckInt     = 300
	defaultHealthInt    = 60
	defaultLoadCheckInt = 900
	pingTarget          = "8.8.8.8"
	apiBaseURL          = "https://api.protonmail.ch"
)

// Configuration
var (
	targetCities       []string
	targetCountry      string
	sessionFile        string
	logDir             string
	cacheDir           string
	protonUser         string
	protonPass         string
	checkInterval      int
	healthCheckInterval int
	loadCheckInterval  int

	// Docker Configuration
	gluetunService   string
	gluetunContainer string
	envFile          string
)

// VPN Server Structs (matching Proton API JSON)
type LogicalServer struct {
	ID           string   `json:"ID"`
	Name         string   `json:"Name"`
	EntryCountry string   `json:"EntryCountry"`
	ExitCountry  string   `json:"ExitCountry"`
	Domain       string   `json:"Domain"`
	Tier         int      `json:"Tier"`
	Features     int      `json:"Features"`
	Status       int      `json:"Status"` // 1 = Active
	Load         int      `json:"Load"`
	Score        float64  `json:"Score"`
	City         string   `json:"City"`
	Servers      []Server `json:"Servers"`
}

type Server struct {
	EntryIP         string `json:"EntryIP"`
	ExitIP          string `json:"ExitIP"`
	Domain          string `json:"Domain"`
	ID              string `json:"ID"`
	Status          int    `json:"Status"`
	X25519PublicKey string `json:"X25519PublicKey"` // WireGuard Key
}

type LogicalServersResponse struct {
	Code           int             `json:"Code"`
	LogicalServers []LogicalServer `json:"LogicalServers"`
}

// Session Data to persist to disk
type SessionData struct {
	UID          string `json:"uid"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func init() {
	// Load Env Vars
	citiesEnv := os.Getenv("TARGET_CITIES")
	if citiesEnv == "" {
		citiesEnv = "San Jose"
	}
	targetCities = strings.Split(citiesEnv, ",")

	targetCountry = os.Getenv("TARGET_COUNTRY")
	sessionFile = getEnv("SESSION_FILE", "/data/proton_session.json")
	logDir = getEnv("LOG_DIR", "/tmp/proton_sidecar/logs")
	cacheDir = getEnv("CACHE_DIR", "/tmp/proton_sidecar/cache")
	protonUser = os.Getenv("PROTON_USERNAME")
	protonPass = os.Getenv("PROTON_PASSWORD")

	checkInterval = getEnvInt("CHECK_INTERVAL", defaultCheckInt)
	healthCheckInterval = getEnvInt("HEALTH_CHECK_INTERVAL", defaultHealthInt)
	loadCheckInterval = getEnvInt("LOAD_CHECK_INTERVAL", defaultLoadCheckInt)

	// Docker Config
	gluetunService = getEnv("GLUETUN_SERVICE_NAME", "gluetun")
	gluetunContainer = getEnv("GLUETUN_CONTAINER_NAME", "gluetun")
	envFile = getEnv("ENV_FILE_PATH", "/project/.env")
}

func main() {
	// Flags
	checkOnly := flag.Bool("check-only", false, "Fetch best server and exit")
	listCities := flag.Bool("list-cities", false, "List all available cities and exit")
	countryFilter := flag.String("country", "", "Filter by country code (e.g. US)")
	flag.Parse()

	log("VPN Manager Started")

	if *listCities {
		runListCities(*countryFilter)
		return
	}

	// Main Manager Logic
	manager := NewProtonManager()
	
	if *checkOnly {
		runCheckOnly(manager)
		return
	}

	// Main Loop
	runDaemon(manager)
}

// --- Manager Logic ---

type ProtonManager struct {
	client       *proton.Client
	apiManager   *proton.Manager
	accessToken  string
	uid          string
	refreshToken string
}

func NewProtonManager() *ProtonManager {
	pm := &ProtonManager{}
	pm.ensureDirs()
	pm.initSession()
	return pm
}

func (pm *ProtonManager) ensureDirs() {
	os.MkdirAll(logDir, 0755)
	os.MkdirAll(cacheDir, 0755)
	
	sessionDir := getDir(sessionFile)
	if _, err := os.Stat(sessionDir); os.IsNotExist(err) {
		os.MkdirAll(sessionDir, 0700)
	}
}

func (pm *ProtonManager) initSession() {
	pm.apiManager = proton.New(
		proton.WithAppVersion("Other"),
	)

	// 1. Try to load from disk
	if err := pm.loadSession(); err == nil {
		log("Session loaded from disk.")
		// Verify session by creating a client
		// We use NewClientWithRefresh to ensure the tokens are valid/refreshed
		ctx := context.Background()
		c, auth, err := pm.apiManager.NewClientWithRefresh(ctx, pm.uid, pm.refreshToken)
		if err == nil {
			pm.client = c
			pm.accessToken = auth.AccessToken
			pm.refreshToken = auth.RefreshToken
			pm.saveSession() // Save potential refresh
			log("Session verified and refreshed.")
			return
		}
		log(fmt.Sprintf("Failed to refresh session: %v. Starting fresh.", err))
	}

	// 2. Fresh Auth
	pm.authenticate()
}

func (pm *ProtonManager) authenticate() {
	if protonUser == "" || protonPass == "" {
		log("Error: PROTON_USERNAME and PROTON_PASSWORD must be set.")
		os.Exit(1)
	}

	log(fmt.Sprintf("Authenticating as %s...", protonUser))
	ctx := context.Background()
	
	// SRP Auth
	c, auth, err := pm.apiManager.NewClientWithLogin(ctx, protonUser, []byte(protonPass))
	if err != nil {
		log(fmt.Sprintf("Authentication failed: %v", err))
		os.Exit(1)
	}

	pm.client = c
	pm.uid = auth.UID
	pm.accessToken = auth.AccessToken
	pm.refreshToken = auth.RefreshToken
	
	log("Authentication successful.")
	pm.saveSession()
}

func (pm *ProtonManager) loadSession() error {
	f, err := os.Open(sessionFile)
	if err != nil {
		return err
	}
	defer f.Close()

	var data SessionData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return err
	}

	pm.uid = data.UID
	pm.accessToken = data.AccessToken
	pm.refreshToken = data.RefreshToken
	return nil
}

func (pm *ProtonManager) saveSession() {
	data := SessionData{
		UID:          pm.uid,
		AccessToken:  pm.accessToken,
		RefreshToken: pm.refreshToken,
	}

	f, err := os.Create(sessionFile)
	if err != nil {
		log(fmt.Sprintf("Failed to save session: %v", err))
		return
	}
	defer f.Close()

	json.NewEncoder(f).Encode(data)
}

// Fetch Servers using standard HTTP client with our AccessToken
func (pm *ProtonManager) getServers() ([]LogicalServer, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", apiBaseURL+"/vpn/logicals", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+pm.accessToken)
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", pm.uid)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		// Token expired, refresh and retry once
		log("Token expired (401). Refreshing...")
		if err := pm.refreshSession(); err == nil {
			req.Header.Set("Authorization", "Bearer "+pm.accessToken)
			resp, err = client.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
		} else {
			return nil, fmt.Errorf("failed to refresh session: %v", err)
		}
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var result LogicalServersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.LogicalServers, nil
}

func (pm *ProtonManager) refreshSession() error {
	ctx := context.Background()
	// We close the old client if it exists to clean up
	if pm.client != nil {
		pm.client.Close()
	}

	c, auth, err := pm.apiManager.NewClientWithRefresh(ctx, pm.uid, pm.refreshToken)
	if err != nil {
		// If refresh fails, try full re-auth
		log("Refresh failed, attempting full re-authentication...")
		// Use authenticate() but handle potential exit
		// Since authenticate() exits on failure, this is fine for now
		pm.authenticate()
		return nil 
	}

	pm.client = c
	pm.accessToken = auth.AccessToken
	pm.refreshToken = auth.RefreshToken
	pm.saveSession()
	return nil
}


// --- CLI Modes ---

func runListCities(countryFilter string) {
	// For listing cities, we need a manager to get servers
	pm := NewProtonManager()
	servers, err := pm.getServers()
	if err != nil {
		log(fmt.Sprintf("Error fetching servers: %v", err))
		os.Exit(1)
	}

	stats := make(map[string]struct {
		Count int
		Load  int
	})

	for _, s := range servers {
		if s.Status != 1 {
			continue
		}
		if countryFilter != "" && !strings.EqualFold(s.EntryCountry, countryFilter) {
			continue
		}
		key := fmt.Sprintf("%s|%s", s.EntryCountry, s.City)
		entry := stats[key]
		entry.Count++
		entry.Load += s.Load
		stats[key] = entry
	}

	// Sort and Print
	var keys []string
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Println("------------------------------------------------------------")
	fmt.Printf("%-8s %-30s %-10s %-10s\n", "COUNTRY", "CITY", "SERVERS", "AVG LOAD")
	fmt.Println("------------------------------------------------------------")

	for _, k := range keys {
		parts := strings.Split(k, "|")
		country, city := parts[0], parts[1]
		data := stats[k]
		avgLoad := 0
		if data.Count > 0 {
			avgLoad = data.Load / data.Count
		}
		fmt.Printf("%-8s %-30s %-10d %d%%\n", country, city, data.Count, avgLoad)
	}
	fmt.Println("------------------------------------------------------------")
}

func runCheckOnly(pm *ProtonManager) {
	log("Running in CHECK ONLY mode...")
	servers, err := pm.getServers()
	if err != nil {
		log(fmt.Sprintf("Error: %v", err))
		os.Exit(1)
	}

	currentName := getCurrentServerFromEnv()
	best, _ := findBestServer(servers, currentName)

	if best != nil {
		fmt.Printf("\n--- REPORT ---\n")
		fmt.Printf("Current Server (Env): %s\n", currentName)
		fmt.Printf("Best Server: %s (Load: %d%%)\n", best.Name, best.Load)
		fmt.Printf("--------------\n")
	} else {
		fmt.Println("No suitable servers found.")
	}
}


// --- Daemon Logic ---

func runDaemon(pm *ProtonManager) {
	lastHealth := time.Time{}
	lastLoad := time.Time{}

	for {
		now := time.Now()

		// 1. Health Check
		if now.Sub(lastHealth) >= time.Duration(healthCheckInterval)*time.Second {
			lastHealth = now
			healthy := checkConnectivity()
			
			if !healthy {
				log("Unhealthy connection detected! Initiating failover...")
				// Force immediate load check to switch
				lastLoad = time.Time{} 
			} else {
				// If healthy, wait before checking load
				if now.Sub(lastLoad) < time.Duration(loadCheckInterval)*time.Second {
					time.Sleep(5 * time.Second)
					continue
				}
			}
		}

		// 2. Load Check / Failover
		if now.Sub(lastLoad) >= time.Duration(loadCheckInterval)*time.Second {
			lastLoad = now
			
			servers, err := pm.getServers()
			if err != nil {
				log(fmt.Sprintf("Error fetching servers: %v", err))
				time.Sleep(30 * time.Second)
				continue
			}

			currentName := getCurrentServerFromEnv()
			healthy := checkConnectivity()
			
			best, currentLoad := findBestServer(servers, currentName)
			
			// Logging
			status := "BAD"
			if healthy { status = "OK" }
			msg := fmt.Sprintf("Health: %s | Current: %s (%d%%)", status, currentName, currentLoad)
			if best != nil {
				msg += fmt.Sprintf(" | Best: %s (%d%%)", best.Name, best.Load)
			}
			log(msg)

			// Decision
			shouldSwitch := false
			target := ""
			reason := ""

			if !healthy {
				shouldSwitch = true
				reason = "Unhealthy Connection"
				if best != nil {
					target = best.Name
				}
			} else if best != nil && currentName != "" {
				if currentLoad > (best.Load + 20) {
					shouldSwitch = true
					target = best.Name
					reason = fmt.Sprintf("Load Optimization (%d%% > %d%% + 20%%)", currentLoad, best.Load)
				}
			}

			if shouldSwitch && target != "" && target != currentName {
				log(fmt.Sprintf("Initiating switch to %s. Reason: %s", target, reason))
				if updateEnv(best) {
					restartGluetun()
					// Wait for restart
					time.Sleep(45 * time.Second)
					// Reset timers
					lastHealth = time.Now()
					lastLoad = time.Now()
				}
			}
		}

		time.Sleep(5 * time.Second)
	}
}


// --- Helpers ---

func findBestServer(servers []LogicalServer, currentName string) (*LogicalServer, int) {
	var candidates []LogicalServer
	currentLoad := 100

	for _, s := range servers {
		if s.Name == currentName {
			currentLoad = s.Load
		}

		if s.Status != 1 {
			continue
		}

		if targetCountry != "" && s.EntryCountry != targetCountry {
			continue
		}

		// Check city match
		cityMatch := false
		for _, city := range targetCities {
			if strings.EqualFold(s.City, strings.TrimSpace(city)) {
				cityMatch = true
				break
			}
		}
		
		if cityMatch {
			candidates = append(candidates, s)
		}
	}

	if len(candidates) == 0 {
		return nil, currentLoad
	}

	// Sort by Load ASC
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Load < candidates[j].Load
	})

	return &candidates[0], currentLoad
}

func checkConnectivity() bool {
	// Use gluetunContainer (name) for docker exec
	cmd := exec.Command("docker", "exec", "-i", gluetunContainer, "ping", "-c", "3", "-W", "2", pingTarget)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func getCurrentServerFromEnv() string {
	data, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "PROTON_SERVER_NAME=") {
			return strings.TrimPrefix(line, "PROTON_SERVER_NAME=")
		}
	}
	return ""
}

func updateEnv(server *LogicalServer) bool {
	// Find WireGuard Key
	var wgServer *Server
	for _, s := range server.Servers {
		if s.X25519PublicKey != "" {
			wgServer = &s
			break
		}
	}

	if wgServer == nil {
		log(fmt.Sprintf("Error: No WireGuard key found for server %s", server.Name))
		return false
	}

	log(fmt.Sprintf("Updating ENV: Name=%s, IP=%s", server.Name, wgServer.EntryIP))

	// Read existing
	content, _ := os.ReadFile(envFile)
	lines := strings.Split(string(content), "\n")
	
	newLines := []string{}
	managedVars := map[string]string{
		"PROTON_SERVER_NAME":      server.Name,
		"WIREGUARD_ENDPOINT_IP":   wgServer.EntryIP,
		"WIREGUARD_ENDPOINT_PORT": "51820",
		"WIREGUARD_PUBLIC_KEY":    wgServer.X25519PublicKey,
	}
	foundVars := make(map[string]bool)

	for _, line := range lines {
		updated := false
		for k, v := range managedVars {
			if strings.HasPrefix(line, k+"=") {
				newLines = append(newLines, fmt.Sprintf("%s=%s", k, v))
				foundVars[k] = true
				updated = true
				break
			}
		}
		if !updated {
			newLines = append(newLines, line)
		}
	}

	// Append missing
	for k, v := range managedVars {
		if !foundVars[k] {
			newLines = append(newLines, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Write back
	output := strings.Join(newLines, "\n")
	// Ensure newline at end
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	
	if err := os.WriteFile(envFile, []byte(output), 0644); err != nil {
		log(fmt.Sprintf("Error updating env: %v", err))
		return false
	}
	return true
}

func restartGluetun() {
	log("Recreating Gluetun...")
	
	// Use gluetunService for docker-compose up
	cmdArgs := []string{"up", "-d", "--force-recreate", gluetunService}
	cmd := exec.Command("docker-compose", cmdArgs...)
	
	if _, err := os.Stat("/project/docker-compose.yml"); err == nil {
		cmd = exec.Command("docker-compose", "-f", "/project/docker-compose.yml", "up", "-d", "--force-recreate", gluetunService)
	}

	if output, err := cmd.CombinedOutput(); err != nil {
		log(fmt.Sprintf("Failed to recreate gluetun: %v\nOutput: %s", err, string(output)))
		// Fallback - Use gluetunContainer for direct docker restart
		exec.Command("docker", "restart", gluetunContainer).Run()
	}
}

func log(msg string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var i int
		fmt.Sscanf(v, "%d", &i)
		return i
	}
	return fallback
}

func getDir(path string) string {
	// naive dirname
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		return "."
	}
	return path[:lastSlash]
}
