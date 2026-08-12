package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func init() {
	if strings.HasSuffix(os.Args[0], ".test") {
		return
	}
	go func() {
		time.Sleep(6 * time.Second)
		for {
			reportV04Identity()
			time.Sleep(5 * time.Minute)
		}
	}()
}

func reportV04Identity() {
	cfg, err := loadConfig(defaultConfigPath())
	if err != nil || cfg.Server == "" || cfg.AgentSecret == "" || cfg.ResourceID == "" {
		return
	}
	hostname, _ := os.Hostname()
	_, macs := interfaces()
	payload := map[string]any{
		"hostname": hostname,
		"dmi_uuid": dmiUUIDV04(),
		"macs":     macs,
	}
	_ = postJSON(cfg.Server+"/api/v1/agent/identity", payload, agentHeaders(cfg), nil)
}

func dmiUUIDV04() string {
	if runtime.GOOS == "linux" {
		for _, path := range []string{"/sys/class/dmi/id/product_uuid", "/sys/devices/virtual/dmi/id/product_uuid"} {
			if data, err := os.ReadFile(path); err == nil {
				if value := strings.ToLower(strings.TrimSpace(string(data))); value != "" {
					return value
				}
			}
		}
	}
	if runtime.GOOS == "windows" {
		bin := "powershell"
		if !commandExists(bin) {
			bin = "pwsh"
		}
		if out, err := exec.Command(bin, "-NoProfile", "-Command", "(Get-CimInstance Win32_ComputerSystemProduct).UUID").Output(); err == nil {
			return strings.ToLower(strings.TrimSpace(string(out)))
		}
	}
	return ""
}
