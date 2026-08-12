package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type proxmoxIntegration struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	BaseURL        string     `json:"base_url"`
	TokenID        string     `json:"token_id"`
	TokenSecretEnc string     `json:"-"`
	VerifyTLS      bool       `json:"verify_tls"`
	Enabled        bool       `json:"enabled"`
	Status         string     `json:"status"`
	LastError      string     `json:"last_error"`
	LastSyncAt     *time.Time `json:"last_sync_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

type proxmoxResource struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	Node     string  `json:"node"`
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	CPU      float64 `json:"cpu"`
	Mem      float64 `json:"mem"`
	MaxMem   float64 `json:"maxmem"`
	Disk     float64 `json:"disk"`
	MaxDisk  float64 `json:"maxdisk"`
	Uptime   int64   `json:"uptime"`
	Template int     `json:"template"`
	Storage  string  `json:"storage"`
	Plugin   string  `json:"plugintype"`
	Shared   int     `json:"shared"`
	Level    string  `json:"level"`
	Tags     string  `json:"tags"`
	Pool     string  `json:"pool"`
	MaxCPU   float64 `json:"maxcpu"`
}

type proxmoxAPI struct {
	baseURL string
	tokenID string
	secret  string
	client  *http.Client
}

func migrateV04(ctx context.Context, db *pgxpool.Pool) error {
	const schema = `
CREATE TABLE IF NOT EXISTS resource_identities (
  id TEXT PRIMARY KEY,
  resource_id TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  kind TEXT NOT NULL,
  value TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(source,kind,value)
);
CREATE INDEX IF NOT EXISTS resource_identities_resource_idx ON resource_identities(resource_id);
CREATE TABLE IF NOT EXISTS proxmox_integrations (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  base_url TEXT NOT NULL,
  token_id TEXT NOT NULL,
  token_secret_enc TEXT NOT NULL,
  verify_tls BOOLEAN NOT NULL DEFAULT true,
  enabled BOOLEAN NOT NULL DEFAULT true,
  status TEXT NOT NULL DEFAULT 'unknown',
  last_error TEXT NOT NULL DEFAULT '',
  last_sync_at TIMESTAMPTZ,
  created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS proxmox_integrations_url_token_idx ON proxmox_integrations(base_url,token_id);`
	_, err := db.Exec(ctx, schema)
	return err
}

func registerV04Routes(mux *http.ServeMux, s *server) {
	mux.Handle("GET /api/v1/topology/v2", s.requireAuth(http.HandlerFunc(s.topologyV2)))
	mux.Handle("GET /api/v1/integrations/proxmox", s.requireAuth(http.HandlerFunc(s.listProxmoxIntegrations)))
	mux.Handle("POST /api/v1/integrations/proxmox", s.requireAuth(http.HandlerFunc(s.createProxmoxIntegration)))
	mux.Handle("DELETE /api/v1/integrations/proxmox/{id}", s.requireAuth(http.HandlerFunc(s.deleteProxmoxIntegration)))
	mux.Handle("POST /api/v1/integrations/proxmox/{id}/sync", s.requireAuth(http.HandlerFunc(s.syncProxmoxNow)))
	mux.Handle("POST /api/v1/proxmox/resources/{id}/action", s.requireAuth(http.HandlerFunc(s.proxmoxAction)))
	mux.Handle("GET /api/v1/proxmox/resources/{id}/snapshots", s.requireAuth(http.HandlerFunc(s.proxmoxSnapshots)))
}

func (s *server) startV04Background(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(90 * time.Second)
		defer ticker.Stop()
		time.Sleep(12 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.syncAllProxmox(context.Background())
			}
		}
	}()
}

func (s *server) syncAllProxmox(ctx context.Context) {
	rows, err := s.db.Query(ctx, "SELECT id FROM proxmox_integrations WHERE enabled=true")
	if err != nil {
		logV04("list integrations for background sync: %v", err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if err := s.syncProxmoxIntegration(ctx, id); err != nil {
			logV04("Proxmox sync %s: %v", id, err)
		}
	}
}

func (s *server) listProxmoxIntegrations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT id,name,base_url,token_id,verify_tls,enabled,status,last_error,last_sync_at,created_at FROM proxmox_integrations ORDER BY name`)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	out := []proxmoxIntegration{}
	for rows.Next() {
		var v proxmoxIntegration
		if err := rows.Scan(&v.ID, &v.Name, &v.BaseURL, &v.TokenID, &v.VerifyTLS, &v.Enabled, &v.Status, &v.LastError, &v.LastSyncAt, &v.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

func (s *server) createProxmoxIntegration(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey{}).(authUser)
	var in struct {
		Name        string `json:"name"`
		BaseURL     string `json:"base_url"`
		TokenID     string `json:"token_id"`
		TokenSecret string `json:"token_secret"`
		VerifyTLS   *bool  `json:"verify_tls"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.TokenID = strings.TrimSpace(in.TokenID)
	if in.Name == "" || in.BaseURL == "" || in.TokenID == "" || in.TokenSecret == "" {
		writeJSON(w, 400, map[string]any{"error": "name, base_url, token_id and token_secret are required"})
		return
	}
	parsed, err := url.Parse(in.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		writeJSON(w, 400, map[string]any{"error": "base_url must be an http:// or https:// Proxmox URL, usually https://HOST:8006"})
		return
	}
	verifyTLS := true
	if in.VerifyTLS != nil {
		verifyTLS = *in.VerifyTLS
	}
	secretEnc, err := sealAxiomSecret(in.TokenSecret)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	client := newProxmoxAPI(in.BaseURL, in.TokenID, in.TokenSecret, verifyTLS)
	var versionInfo map[string]any
	if err := client.get(r.Context(), "/version", &versionInfo); err != nil {
		writeJSON(w, 400, map[string]any{"error": "unable to connect to Proxmox: " + err.Error()})
		return
	}
	id := newID("pxm")
	_, err = s.db.Exec(r.Context(), `INSERT INTO proxmox_integrations(id,name,base_url,token_id,token_secret_enc,verify_tls,created_by,status) VALUES($1,$2,$3,$4,$5,$6,$7,'connected')`, id, in.Name, in.BaseURL, in.TokenID, secretEnc, verifyTLS, u.ID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if err := s.syncProxmoxIntegration(r.Context(), id); err != nil {
		logV04("initial Proxmox sync failed: %v", err)
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": in.Name, "base_url": in.BaseURL, "status": "connected"})
}

func (s *server) deleteProxmoxIntegration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback(r.Context())
	_, _ = tx.Exec(r.Context(), `UPDATE resources SET status='removed',updated_at=now() WHERE source='proxmox' AND metadata->>'integration_id'=$1`, id)
	result, err := tx.Exec(r.Context(), "DELETE FROM proxmox_integrations WHERE id=$1", id)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if result.RowsAffected() == 0 {
		writeJSON(w, 404, map[string]any{"error": "Proxmox integration not found"})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) syncProxmoxNow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.syncProxmoxIntegration(r.Context(), id); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "synced_at": time.Now()})
}

func (s *server) loadProxmoxIntegration(ctx context.Context, id string) (proxmoxIntegration, string, error) {
	var p proxmoxIntegration
	if err := s.db.QueryRow(ctx, `SELECT id,name,base_url,token_id,token_secret_enc,verify_tls,enabled,status,last_error,last_sync_at,created_at FROM proxmox_integrations WHERE id=$1`, id).Scan(&p.ID, &p.Name, &p.BaseURL, &p.TokenID, &p.TokenSecretEnc, &p.VerifyTLS, &p.Enabled, &p.Status, &p.LastError, &p.LastSyncAt, &p.CreatedAt); err != nil {
		return p, "", err
	}
	secret, err := openAxiomSecret(p.TokenSecretEnc)
	return p, secret, err
}

func (s *server) syncProxmoxIntegration(ctx context.Context, id string) error {
	integration, secret, err := s.loadProxmoxIntegration(ctx, id)
	if err != nil {
		return err
	}
	api := newProxmoxAPI(integration.BaseURL, integration.TokenID, secret, integration.VerifyTLS)
	var resources []proxmoxResource
	if err := api.get(ctx, "/cluster/resources", &resources); err != nil {
		_, _ = s.db.Exec(ctx, "UPDATE proxmox_integrations SET status='error',last_error=$2,updated_at=now() WHERE id=$1", id, limitString(err.Error(), 4096))
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	clusterID, err := ensureProxmoxIdentityResource(ctx, tx, id, "cluster", id, "proxmox_cluster", integration.Name, "online", false, map[string]any{
		"integration_id": id,
		"base_url":       integration.BaseURL,
		"proxmox":        true,
	})
	if err != nil {
		return err
	}

	nodeIDs := map[string]string{}
	for _, item := range resources {
		if item.Type != "node" {
			continue
		}
		status := normalizeProxmoxStatus(item.Status)
		meta := map[string]any{"integration_id": id, "proxmox": true, "node": item.Node, "uptime_seconds": item.Uptime}
		nodeID, err := ensureProxmoxIdentityResource(ctx, tx, id, "node", item.Node, "proxmox_node", item.Node, status, false, meta)
		if err != nil {
			return err
		}
		nodeIDs[item.Node] = nodeID
		if err := ensureRelationship(ctx, tx, clusterID, nodeID, "CONTAINS"); err != nil {
			return err
		}
		if err := upsertResourceMetrics(ctx, tx, nodeID, item.CPU*100, percentOf(item.Mem, item.MaxMem), percentOf(item.Disk, item.MaxDisk), item.Uptime); err != nil {
			return err
		}
	}

	for _, item := range resources {
		switch item.Type {
		case "qemu", "lxc":
			if item.VMID == 0 || item.Node == "" {
				continue
			}
			config := map[string]any{}
			_ = api.get(ctx, fmt.Sprintf("/nodes/%s/%s/%d/config", url.PathEscape(item.Node), item.Type, item.VMID), &config)
			dmiUUID := extractDMIUUID(config)
			macs := extractProxmoxMACs(config)
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = fmt.Sprintf("%s-%d", item.Type, item.VMID)
			}
			metadata := map[string]any{
				"integration_id": id,
				"proxmox":        true,
				"node":           item.Node,
				"guest_type":     item.Type,
				"vmid":           item.VMID,
				"proxmox_status": item.Status,
				"template":       item.Template == 1,
				"tags":           item.Tags,
				"pool":           item.Pool,
				"dmi_uuid":       dmiUUID,
				"macs":           macs,
				"max_memory":     int64(item.MaxMem),
				"max_disk":       int64(item.MaxDisk),
				"max_cpu":        item.MaxCPU,
				"uptime_seconds": item.Uptime,
			}
			for _, key := range []string{"cores", "sockets", "memory", "ostype", "description", "bios", "machine", "agent", "onboot", "startup"} {
				if value, ok := config[key]; ok {
					metadata[key] = value
				}
			}
			typ := "virtual_machine"
			if item.Type == "lxc" {
				typ = "lxc"
			}
			identityValue := fmt.Sprintf("%s:%s:%d", item.Node, item.Type, item.VMID)
			guestID, err := s.ensureProxmoxGuest(ctx, tx, integration.ID, identityValue, typ, name, normalizeProxmoxStatus(item.Status), metadata, dmiUUID, macs)
			if err != nil {
				return err
			}
			if nodeID := nodeIDs[item.Node]; nodeID != "" {
				if err := ensureRelationship(ctx, tx, nodeID, guestID, "HOSTS"); err != nil {
					return err
				}
			}
			if err := upsertResourceMetrics(ctx, tx, guestID, item.CPU*100, percentOf(item.Mem, item.MaxMem), percentOf(item.Disk, item.MaxDisk), item.Uptime); err != nil {
				return err
			}
		case "storage":
			if item.Node == "" || item.Storage == "" {
				continue
			}
			name := item.Storage + " @ " + item.Node
			identityValue := item.Node + ":" + item.Storage
			storageID, err := ensureProxmoxIdentityResource(ctx, tx, integration.ID, "storage", identityValue, "storage", name, normalizeProxmoxStatus(item.Status), false, map[string]any{
				"integration_id": id,
				"proxmox":        true,
				"node":           item.Node,
				"storage":        item.Storage,
				"plugin_type":    item.Plugin,
				"shared":         item.Shared == 1,
				"total_bytes":    int64(item.MaxDisk),
				"used_bytes":     int64(item.Disk),
			})
			if err != nil {
				return err
			}
			if nodeID := nodeIDs[item.Node]; nodeID != "" {
				if err := ensureRelationship(ctx, tx, nodeID, storageID, "USES"); err != nil {
					return err
				}
			}
			if err := upsertResourceMetrics(ctx, tx, storageID, 0, 0, percentOf(item.Disk, item.MaxDisk), 0); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	_, _ = s.db.Exec(ctx, "UPDATE proxmox_integrations SET status='connected',last_error='',last_sync_at=now(),updated_at=now() WHERE id=$1", id)
	return nil
}

func (s *server) ensureProxmoxGuest(ctx context.Context, tx pgx.Tx, integrationID, identityValue, typ, name, status string, metadata map[string]any, dmiUUID string, macs []string) (string, error) {
	if id, err := lookupIdentity(ctx, tx, "proxmox", "guest:"+integrationID, identityValue); err == nil {
		return updateMergedResource(ctx, tx, id, typ, name, status, metadata)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	matchID, err := findStrongResourceMatch(ctx, tx, dmiUUID, macs)
	if err != nil {
		return "", err
	}
	if matchID != "" {
		id, err := updateMergedResource(ctx, tx, matchID, typ, name, status, metadata)
		if err != nil {
			return "", err
		}
		if err := addIdentity(ctx, tx, id, "proxmox", "guest:"+integrationID, identityValue); err != nil {
			return "", err
		}
		return id, nil
	}

	metadataJSON, _ := json.Marshal(metadata)
	id := newID("res")
	if _, err := tx.Exec(ctx, `INSERT INTO resources(id,type,name,status,managed,source,external_id,metadata) VALUES($1,$2,$3,$4,false,'proxmox',$5,$6)`, id, typ, name, status, integrationID+":"+identityValue, metadataJSON); err != nil {
		return "", err
	}
	if err := addIdentity(ctx, tx, id, "proxmox", "guest:"+integrationID, identityValue); err != nil {
		return "", err
	}
	return id, nil
}

func ensureProxmoxIdentityResource(ctx context.Context, tx pgx.Tx, integrationID, kind, value, typ, name, status string, managed bool, metadata map[string]any) (string, error) {
	identityKind := kind + ":" + integrationID
	if id, err := lookupIdentity(ctx, tx, "proxmox", identityKind, value); err == nil {
		metadataJSON, _ := json.Marshal(metadata)
		_, err = tx.Exec(ctx, "UPDATE resources SET type=$2,name=$3,status=$4,metadata=$5,updated_at=now() WHERE id=$1", id, typ, name, status, metadataJSON)
		return id, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	metadataJSON, _ := json.Marshal(metadata)
	id := newID("res")
	if _, err := tx.Exec(ctx, `INSERT INTO resources(id,type,name,status,managed,source,external_id,metadata) VALUES($1,$2,$3,$4,$5,'proxmox',$6,$7)`, id, typ, name, status, managed, integrationID+":"+kind+":"+value, metadataJSON); err != nil {
		return "", err
	}
	if err := addIdentity(ctx, tx, id, "proxmox", identityKind, value); err != nil {
		return "", err
	}
	return id, nil
}

func lookupIdentity(ctx context.Context, tx pgx.Tx, source, kind, value string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, "SELECT resource_id FROM resource_identities WHERE source=$1 AND kind=$2 AND value=$3", source, kind, normalizeIdentity(value)).Scan(&id)
	return id, err
}

func addIdentity(ctx context.Context, tx pgx.Tx, resourceID, source, kind, value string) error {
	value = normalizeIdentity(value)
	if value == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO resource_identities(id,resource_id,source,kind,value) VALUES($1,$2,$3,$4,$5) ON CONFLICT(source,kind,value) DO UPDATE SET resource_id=excluded.resource_id`, newID("ident"), resourceID, source, kind, value)
	return err
}

func (s *server) reconcileAgentResource(ctx context.Context, tx pgx.Tx, machineID, hostname, dmiUUID string, macs []string) (string, map[string]any, error) {
	if id, err := lookupIdentity(ctx, tx, "agent", "machine_id", machineID); err == nil {
		meta, metaErr := resourceMetadata(ctx, tx, id)
		return id, meta, metaErr
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, err
	}
	var legacyID string
	if err := tx.QueryRow(ctx, "SELECT id FROM resources WHERE source='agent' AND external_id=$1", machineID).Scan(&legacyID); err == nil {
		meta, metaErr := resourceMetadata(ctx, tx, legacyID)
		if metaErr == nil {
			_ = addIdentity(ctx, tx, legacyID, "agent", "machine_id", machineID)
		}
		return legacyID, meta, metaErr
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, err
	}
	matchID, err := findStrongResourceMatch(ctx, tx, dmiUUID, macs)
	if err != nil {
		return "", nil, err
	}
	if matchID != "" {
		meta, err := resourceMetadata(ctx, tx, matchID)
		return matchID, meta, err
	}
	return "", map[string]any{}, nil
}

func findStrongResourceMatch(ctx context.Context, tx pgx.Tx, dmiUUID string, macs []string) (string, error) {
	dmiUUID = normalizeIdentity(dmiUUID)
	macSet := map[string]bool{}
	for _, mac := range macs {
		if normalized := normalizeMAC(mac); normalized != "" {
			macSet[normalized] = true
		}
	}
	rows, err := tx.Query(ctx, `SELECT id,metadata FROM resources WHERE type IN ('physical_host','virtual_machine','lxc') AND status <> 'removed'`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type candidate struct{ id string; score int }
	var candidates []candidate
	for rows.Next() {
		var id string
		var raw []byte
		if rows.Scan(&id, &raw) != nil {
			continue
		}
		meta := map[string]any{}
		_ = json.Unmarshal(raw, &meta)
		score := 0
		if dmiUUID != "" && normalizeIdentity(asString(meta["dmi_uuid"])) == dmiUUID {
			score = 100
		}
		for _, existing := range anyStrings(meta["macs"]) {
			if macSet[normalizeMAC(existing)] {
				if score < 90 {
					score = 90
				}
			}
		}
		if score >= 90 {
			candidates = append(candidates, candidate{id: id, score: score})
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	if len(candidates) > 1 && candidates[0].score == candidates[1].score {
		return "", nil
	}
	return candidates[0].id, nil
}

func updateMergedResource(ctx context.Context, tx pgx.Tx, id, typ, name, status string, incoming map[string]any) (string, error) {
	var raw []byte
	var managed bool
	if err := tx.QueryRow(ctx, "SELECT metadata,managed FROM resources WHERE id=$1", id).Scan(&raw, &managed); err != nil {
		return "", err
	}
	meta := map[string]any{}
	_ = json.Unmarshal(raw, &meta)
	for key, value := range incoming {
		if value != nil && asString(value) != "" {
			meta[key] = value
		} else if _, ok := meta[key]; !ok {
			meta[key] = value
		}
	}
	var agentConnected bool
	_ = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agents WHERE resource_id=$1)", id).Scan(&agentConnected)
	meta["agent_connected"] = agentConnected
	metadataJSON, _ := json.Marshal(meta)
	_, err := tx.Exec(ctx, "UPDATE resources SET type=$2,name=$3,status=$4,managed=$5,metadata=$6,updated_at=now() WHERE id=$1", id, typ, name, status, managed, metadataJSON)
	return id, err
}

func resourceMetadata(ctx context.Context, tx pgx.Tx, id string) (map[string]any, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT metadata FROM resources WHERE id=$1", id).Scan(&raw); err != nil {
		return nil, err
	}
	meta := map[string]any{}
	_ = json.Unmarshal(raw, &meta)
	return meta, nil
}

func upsertResourceMetrics(ctx context.Context, tx pgx.Tx, id string, cpu, memory, disk float64, uptime int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO metrics_latest(resource_id,cpu_percent,memory_percent,disk_percent,load1,uptime_seconds,updated_at) VALUES($1,$2,$3,$4,0,$5,now()) ON CONFLICT(resource_id) DO UPDATE SET cpu_percent=excluded.cpu_percent,memory_percent=excluded.memory_percent,disk_percent=excluded.disk_percent,uptime_seconds=excluded.uptime_seconds,updated_at=now()`, id, clamp(cpu), clamp(memory), clamp(disk), uptime)
	return err
}

func (s *server) proxmoxAction(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey{}).(authUser)
	resourceID := r.PathValue("id")
	var in struct{ Action string `json:"action"` }
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	if !map[string]bool{"start": true, "stop": true, "shutdown": true, "reboot": true}[in.Action] {
		writeJSON(w, 400, map[string]any{"error": "unsupported Proxmox action"})
		return
	}
	var typ string
	var managed bool
	var raw []byte
	if err := s.db.QueryRow(r.Context(), "SELECT type,managed,metadata FROM resources WHERE id=$1", resourceID).Scan(&typ, &managed, &raw); err != nil || (typ != "virtual_machine" && typ != "lxc") {
		writeJSON(w, 404, map[string]any{"error": "Proxmox guest not found"})
		return
	}
	if !managed {
		writeJSON(w, 403, map[string]any{"error": "guest is unmanaged; enable management before running power actions"})
		return
	}
	meta := map[string]any{}
	_ = json.Unmarshal(raw, &meta)
	integrationID := asString(meta["integration_id"])
	node := asString(meta["node"])
	guestType := asString(meta["guest_type"])
	vmid := asInt(meta["vmid"])
	integration, secret, err := s.loadProxmoxIntegration(r.Context(), integrationID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	api := newProxmoxAPI(integration.BaseURL, integration.TokenID, secret, integration.VerifyTLS)
	var task string
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/%s", url.PathEscape(node), guestType, vmid, in.Action)
	if err := api.post(r.Context(), path, url.Values{}, &task); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	_, _ = s.db.Exec(r.Context(), "INSERT INTO activity(id,actor,action,resource_id,detail) VALUES($1,$2,$3,$4,jsonb_build_object('task',$5,'source','proxmox'))", newID("act"), "user:"+u.ID, "proxmox_"+in.Action, resourceID, task)
	writeJSON(w, 202, map[string]any{"ok": true, "task": task, "action": in.Action})
}

func (s *server) proxmoxSnapshots(w http.ResponseWriter, r *http.Request) {
	resourceID := r.PathValue("id")
	var typ string
	var raw []byte
	if err := s.db.QueryRow(r.Context(), "SELECT type,metadata FROM resources WHERE id=$1", resourceID).Scan(&typ, &raw); err != nil || (typ != "virtual_machine" && typ != "lxc") {
		writeJSON(w, 404, map[string]any{"error": "Proxmox guest not found"})
		return
	}
	meta := map[string]any{}
	_ = json.Unmarshal(raw, &meta)
	integration, secret, err := s.loadProxmoxIntegration(r.Context(), asString(meta["integration_id"]))
	if err != nil {
		writeError(w, 500, err)
		return
	}
	api := newProxmoxAPI(integration.BaseURL, integration.TokenID, secret, integration.VerifyTLS)
	var snapshots []map[string]any
	path := fmt.Sprintf("/nodes/%s/%s/%d/snapshot", url.PathEscape(asString(meta["node"])), asString(meta["guest_type"]), asInt(meta["vmid"]))
	if err := api.get(r.Context(), path, &snapshots); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, snapshots)
}

func (s *server) topologyV2(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT r.id,r.type,r.name,r.status,r.managed,r.source,r.metadata,m.cpu_percent,m.memory_percent,m.disk_percent,EXISTS(SELECT 1 FROM agents a WHERE a.resource_id=r.id),COALESCE((SELECT jsonb_agg(jsonb_build_object('source',ri.source,'kind',ri.kind,'value',ri.value)) FROM resource_identities ri WHERE ri.resource_id=r.id),'[]'::jsonb) FROM resources r LEFT JOIN metrics_latest m ON m.resource_id=r.id WHERE r.status <> 'removed' ORDER BY r.type,r.name`)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	nodes := []map[string]any{}
	for rows.Next() {
		var id, typ, name, status, source string
		var managed, agentConnected bool
		var metadataRaw, identitiesRaw []byte
		var cpu, memory, disk *float64
		if err := rows.Scan(&id, &typ, &name, &status, &managed, &source, &metadataRaw, &cpu, &memory, &disk, &agentConnected, &identitiesRaw); err != nil {
			writeError(w, 500, err)
			return
		}
		metadata := map[string]any{}
		_ = json.Unmarshal(metadataRaw, &metadata)
		metadata["agent_connected"] = agentConnected
		var identities []map[string]any
		_ = json.Unmarshal(identitiesRaw, &identities)
		nodes = append(nodes, map[string]any{
			"id": id, "type": typ, "name": name, "status": status, "managed": managed, "source": source,
			"metadata": metadata, "identities": identities,
			"metrics": map[string]any{"cpu_percent": derefFloat(cpu), "memory_percent": derefFloat(memory), "disk_percent": derefFloat(disk)},
		})
	}
	edges := []map[string]any{}
	erows, err := s.db.Query(r.Context(), `SELECT rel.id,rel.source_id,rel.target_id,rel.kind FROM relationships rel JOIN resources s ON s.id=rel.source_id JOIN resources t ON t.id=rel.target_id WHERE s.status <> 'removed' AND t.status <> 'removed'`)
	if err == nil {
		defer erows.Close()
		for erows.Next() {
			var id, source, target, kind string
			if erows.Scan(&id, &source, &target, &kind) == nil {
				edges = append(edges, map[string]any{"id": id, "source": source, "target": target, "kind": kind})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"nodes": nodes, "edges": edges})
}

func newProxmoxAPI(baseURL, tokenID, secret string, verifyTLS bool) *proxmoxAPI {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !verifyTLS}}
	return &proxmoxAPI{
		baseURL: strings.TrimRight(baseURL, "/") + "/api2/json",
		tokenID: tokenID,
		secret:  secret,
		client:  &http.Client{Timeout: 20 * time.Second, Transport: transport},
	}
}

func (p *proxmoxAPI) get(ctx context.Context, path string, out any) error {
	return p.request(ctx, http.MethodGet, path, nil, out)
}

func (p *proxmoxAPI) post(ctx context.Context, path string, form url.Values, out any) error {
	return p.request(ctx, http.MethodPost, path, strings.NewReader(form.Encode()), out)
}

func (p *proxmoxAPI) request(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+p.tokenID+"="+p.secret)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Proxmox returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var envelope struct{ Data json.RawMessage `json:"data"` }
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode Proxmox response: %w", err)
	}
	if out == nil || string(envelope.Data) == "null" || len(envelope.Data) == 0 {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

func sealAxiomSecret(value string) (string, error) {
	key := axiomSecretKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func openAxiomSecret(value string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(axiomSecretKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("encrypted secret is truncated")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(plain), err
}

func axiomSecretKey() []byte {
	value := os.Getenv("AXIOM_SECRET_KEY")
	if value == "" {
		value = "axiom-development-key-change-me"
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func extractDMIUUID(config map[string]any) string {
	value := asString(config["smbios1"])
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "uuid=") {
			return normalizeIdentity(strings.TrimPrefix(part, "uuid="))
		}
	}
	return ""
}

func extractProxmoxMACs(config map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for key, raw := range config {
		if !strings.HasPrefix(key, "net") {
			continue
		}
		value := asString(raw)
		for _, token := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '=' || r == ' ' }) {
			if mac := normalizeMAC(token); mac != "" && strings.Count(mac, ":") == 5 && len(mac) == 17 && !seen[mac] {
				seen[mac] = true
				out = append(out, mac)
			}
		}
	}
	sort.Strings(out)
	return out
}

func normalizeMAC(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "-", ":")))
	parts := strings.Split(value, ":")
	if len(parts) != 6 {
		return ""
	}
	for _, part := range parts {
		if len(part) != 2 {
			return ""
		}
		if _, err := strconv.ParseUint(part, 16, 8); err != nil {
			return ""
		}
	}
	return value
}

func normalizeIdentity(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func normalizeProxmoxStatus(value string) string {
	switch strings.ToLower(value) {
	case "online", "running":
		return "online"
	case "offline":
		return "offline"
	case "stopped":
		return "stopped"
	default:
		if value == "" {
			return "unknown"
		}
		return strings.ToLower(value)
	}
}
func percentOf(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return (used / total) * 100
}
func anyStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case []string:
		return append(out, t...)
	case []any:
		for _, item := range t {
			if s := asString(item); s != "" {
				out = append(out, s)
			}
		}
	case string:
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}
func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(t)
		return i
	default:
		return 0
	}
}
func logV04(format string, args ...any) { fmt.Fprintf(os.Stderr, "[v0.4] "+format+"\n", args...) }
