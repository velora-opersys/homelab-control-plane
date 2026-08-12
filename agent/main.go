package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const version = "0.2.0-dev"

type config struct {
	Server      string `json:"server"`
	ResourceID  string `json:"resource_id"`
	AgentSecret string `json:"agent_secret"`
}

type enrolResponse struct {
	ResourceID  string `json:"resource_id"`
	AgentSecret string `json:"agent_secret"`
}

type dockerContainer struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Image          string  `json:"image"`
	State          string  `json:"state"`
	Status         string  `json:"status"`
	Ports          string  `json:"ports"`
	ComposeProject string  `json:"compose_project,omitempty"`
	ComposeService string  `json:"compose_service,omitempty"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryPercent  float64 `json:"memory_percent"`
}

type queuedCommand struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	ContainerID string         `json:"container_id"`
	Payload     map[string]any `json:"payload"`
}

func main() {
	server := flag.String("server", "", "Axiom URL, e.g. http://192.168.1.10:8787")
	token := flag.String("token", "", "one-time enrolment token")
	configPath := flag.String("config", defaultConfigPath(), "agent configuration path")
	once := flag.Bool("once", false, "send one heartbeat and exit")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("read config: %v", err)
	}
	if cfg.Server == "" && *server != "" {
		cfg.Server = strings.TrimRight(*server, "/")
	}
	if cfg.AgentSecret == "" {
		if cfg.Server == "" || *token == "" {
			log.Fatalf("agent is not enrolled; provide --server and --token")
		}
		cfg, err = enrol(cfg.Server, *token)
		if err != nil {
			log.Fatalf("enrolment failed: %v", err)
		}
		if err := saveConfig(*configPath, cfg); err != nil {
			log.Fatalf("save config: %v", err)
		}
		log.Printf("enrolled as %s", cfg.ResourceID)
	}

	for {
		if err := heartbeat(cfg); err != nil {
			log.Printf("heartbeat failed: %v", err)
		} else {
			log.Printf("heartbeat sent")
		}
		if err := processCommands(cfg); err != nil {
			log.Printf("command poll failed: %v", err)
		}
		if *once {
			return
		}
		time.Sleep(15 * time.Second)
	}
}

func enrol(server, token string) (config, error) {
	hostname, _ := os.Hostname()
	ips, macs := interfaces()
	payload := map[string]any{
		"token":      token,
		"hostname":   hostname,
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"machine_id": machineID(hostname, macs),
		"version":    version,
		"ips":        ips,
		"macs":       macs,
		"capabilities": map[string]any{
			"docker":     commandExists("docker"),
			"systemd":    commandExists("systemctl"),
			"powershell": commandExists("powershell") || commandExists("pwsh"),
		},
	}
	var out enrolResponse
	if err := postJSON(server+"/api/v1/agent/enrol", payload, nil, &out); err != nil {
		return config{}, err
	}
	if out.ResourceID == "" || out.AgentSecret == "" {
		return config{}, fmt.Errorf("server returned incomplete credentials")
	}
	return config{Server: server, ResourceID: out.ResourceID, AgentSecret: out.AgentSecret}, nil
}

func heartbeat(cfg config) error {
	m := collectMetrics()
	m["version"] = version
	if runtime.GOOS == "linux" && commandExists("docker") {
		m["docker"] = discoverDocker()
	}
	return postJSON(cfg.Server+"/api/v1/agent/heartbeat", m, agentHeaders(cfg), nil)
}

func processCommands(cfg config) error {
	var commands []queuedCommand
	if err := getJSON(cfg.Server+"/api/v1/agent/commands", agentHeaders(cfg), &commands); err != nil {
		return err
	}
	for _, cmd := range commands {
		result, err := executeCommand(cmd)
		payload := map[string]any{"success": err == nil, "result": result}
		if err != nil {
			payload["error"] = err.Error()
		}
		if postErr := postJSON(cfg.Server+"/api/v1/agent/commands/"+cmd.ID+"/result", payload, agentHeaders(cfg), nil); postErr != nil {
			log.Printf("report command %s: %v", cmd.ID, postErr)
		}
	}
	return nil
}

func executeCommand(cmd queuedCommand) (string, error) {
	if cmd.ContainerID == "" {
		return "", fmt.Errorf("container id missing")
	}
	var args []string
	switch cmd.Kind {
	case "docker_start":
		args = []string{"start", cmd.ContainerID}
	case "docker_stop":
		args = []string{"stop", cmd.ContainerID}
	case "docker_restart":
		args = []string{"restart", cmd.ContainerID}
	case "docker_logs":
		args = []string{"logs", "--tail", "200", "--timestamps", cmd.ContainerID}
	default:
		return "", fmt.Errorf("unsupported command %q", cmd.Kind)
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 128*1024 {
		text = text[len(text)-128*1024:]
	}
	if err != nil {
		return text, fmt.Errorf("docker command failed: %w", err)
	}
	return text, nil
}

func discoverDocker() []dockerContainer {
	out, err := exec.Command("docker", "ps", "-a", "--no-trunc", "--format", "{{json .}}").Output()
	if err != nil {
		return nil
	}
	stats := dockerStats()
	containers := []dockerContainer{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]string
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		id := row["ID"]
		labels := dockerLabels(id)
		c := dockerContainer{
			ID:             id,
			Name:           row["Names"],
			Image:          row["Image"],
			State:          row["State"],
			Status:         row["Status"],
			Ports:          row["Ports"],
			ComposeProject: labels["com.docker.compose.project"],
			ComposeService: labels["com.docker.compose.service"],
		}
		for key, s := range stats {
			if strings.HasPrefix(id, key) || strings.HasPrefix(key, id) || key == c.Name {
				c.CPUPercent = parsePercent(s[0])
				c.MemoryPercent = parsePercent(s[1])
				break
			}
		}
		containers = append(containers, c)
	}
	return containers
}

func dockerLabels(id string) map[string]string {
	out, err := exec.Command("docker", "inspect", "--format", "{{json .Config.Labels}}", id).Output()
	if err != nil {
		return map[string]string{}
	}
	labels := map[string]string{}
	_ = json.Unmarshal(bytes.TrimSpace(out), &labels)
	return labels
}

func dockerStats() map[string][2]string {
	out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{json .}}").Output()
	if err != nil {
		return map[string][2]string{}
	}
	stats := map[string][2]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var row map[string]string
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		key := row["Container"]
		if key == "" {
			key = row["Name"]
		}
		stats[key] = [2]string{row["CPUPerc"], row["MemPerc"]}
		if name := row["Name"]; name != "" {
			stats[name] = [2]string{row["CPUPerc"], row["MemPerc"]}
		}
	}
	return stats
}

func parsePercent(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
	return v
}

func collectMetrics() map[string]any {
	m := map[string]any{"cpu_percent": 0.0, "memory_percent": 0.0, "disk_percent": 0.0, "load1": 0.0, "uptime_seconds": int64(0)}
	if runtime.GOOS == "linux" {
		m["load1"] = linuxLoad1()
		m["memory_percent"] = linuxMemoryPercent()
		m["disk_percent"] = linuxDiskPercent()
		m["uptime_seconds"] = linuxUptime()
		m["cpu_percent"] = linuxCPUPercent()
	} else if runtime.GOOS == "windows" {
		m["memory_percent"] = windowsMetric("(Get-CimInstance Win32_OperatingSystem | ForEach-Object { 100*(1-$_.FreePhysicalMemory/$_.TotalVisibleMemorySize) })")
		m["disk_percent"] = windowsMetric("(Get-CimInstance Win32_LogicalDisk -Filter \"DeviceID='C:'\" | ForEach-Object { 100*(1-$_.FreeSpace/$_.Size) })")
		m["cpu_percent"] = windowsMetric("(Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average")
	}
	return m
}

func linuxCPUPercent() float64 {
	first := readCPUStat()
	time.Sleep(250 * time.Millisecond)
	second := readCPUStat()
	if second.total <= first.total {
		return 0
	}
	total := second.total - first.total
	idle := second.idle - first.idle
	return 100 * (1 - float64(idle)/float64(total))
}

type cpuStat struct{ total, idle uint64 }

func readCPUStat() cpuStat {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuStat{}
	}
	fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
	var s cpuStat
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		s.total += v
		if i == 3 || i == 4 {
			s.idle += v
		}
	}
	return s
}

func linuxLoad1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func linuxMemoryPercent() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	vals := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			vals[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	total, available := vals["MemTotal"], vals["MemAvailable"]
	if total <= 0 {
		return 0
	}
	return 100 * (1 - available/total)
}

func linuxDiskPercent() float64 {
	out, err := exec.Command("df", "-P", "/").Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 5 {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64)
	return v
}

func linuxUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func windowsMetric(script string) float64 {
	bin := "powershell"
	if !commandExists(bin) {
		bin = "pwsh"
	}
	out, err := exec.Command(bin, "-NoProfile", "-Command", script).Output()
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return v
}

func machineID(hostname string, macs []string) string {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/etc/machine-id"); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
	}
	h := sha256.Sum256([]byte(hostname + "|" + strings.Join(macs, ",")))
	return hex.EncodeToString(h[:16])
}

func interfaces() ([]string, []string) {
	var ips, macs []string
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if hw := iface.HardwareAddr.String(); hw != "" {
			macs = append(macs, hw)
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips, macs
}

func agentHeaders(cfg config) map[string]string {
	return map[string]string{"Authorization": "Bearer " + cfg.AgentSecret, "X-Agent-ID": cfg.ResourceID}
}

func postJSON(url string, body any, headers map[string]string, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func getJSON(url string, headers map[string]string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func loadConfig(path string) (config, error) {
	var cfg config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	return cfg, err
}

func saveConfig(path string, cfg config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, b, 0600)
}

func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "Axiom", "agent.json")
	}
	return "/var/lib/axiom-agent/agent.json"
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
