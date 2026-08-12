package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	go startV04Extension()
}

func startV04Extension() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://homelab:homelab@localhost:5432/homelab?sslmode=disable"))
	if err != nil {
		log.Printf("v0.4 extension database configuration: %v", err)
		return
	}
	for i := 0; i < 60; i++ {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		pool.Close()
		log.Printf("v0.4 extension database unavailable: %v", err)
		return
	}
	if err := migrateV04(ctx, pool); err != nil {
		pool.Close()
		log.Printf("v0.4 extension migration failed: %v", err)
		return
	}

	s := &server{db: pool}
	mux := http.NewServeMux()
	registerV04Routes(mux, s)
	mux.HandleFunc("POST /api/v1/agent/identity", s.agentIdentityV04)
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "database": true, "version": "0.4.0-dev", "extension": "proxmox-topology"})
	})

	go s.v04SyncLoop(ctx)
	log.Printf("Axiom v0.4 Proxmox/Topology extension listening on :8081")
	if err := http.ListenAndServe(":8081", requestLogger(mux)); err != nil {
		log.Printf("v0.4 extension stopped: %v", err)
	}
	pool.Close()
}

func (s *server) v04SyncLoop(ctx context.Context) {
	initial := time.NewTimer(8 * time.Second)
	select {
	case <-ctx.Done():
		initial.Stop()
		return
	case <-initial.C:
	}
	s.syncAllProxmox(ctx)
	_ = s.reconcileProxmoxDuplicates(ctx)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncAllProxmox(ctx)
			if err := s.reconcileProxmoxDuplicates(ctx); err != nil {
				log.Printf("v0.4 reconciliation: %v", err)
			}
		}
	}
}

func (s *server) agentIdentityV04(w http.ResponseWriter, r *http.Request) {
	rid, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	var in struct {
		DMIUUID  string   `json:"dmi_uuid"`
		MACs     []string `json:"macs"`
		Hostname string   `json:"hostname"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	if err := tx.QueryRow(r.Context(), "SELECT metadata FROM resources WHERE id=$1", rid).Scan(&raw); err != nil {
		writeError(w, 404, err)
		return
	}
	meta := map[string]any{}
	_ = json.Unmarshal(raw, &meta)
	if in.DMIUUID != "" {
		meta["dmi_uuid"] = normalizeIdentity(in.DMIUUID)
		_ = addIdentity(r.Context(), tx, rid, "agent", "dmi_uuid", in.DMIUUID)
	}
	if len(in.MACs) > 0 {
		clean := make([]string, 0, len(in.MACs))
		for _, value := range in.MACs {
			if mac := normalizeMAC(value); mac != "" {
				clean = append(clean, mac)
				_ = addIdentity(r.Context(), tx, rid, "agent", "mac", mac)
			}
		}
		meta["macs"] = clean
	}
	if strings.TrimSpace(in.Hostname) != "" {
		meta["agent_hostname"] = strings.TrimSpace(in.Hostname)
	}
	meta["agent_connected"] = true
	patched, _ := json.Marshal(meta)
	if _, err := tx.Exec(r.Context(), "UPDATE resources SET metadata=$2,updated_at=now() WHERE id=$1", rid, patched); err != nil {
		writeError(w, 500, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, err)
		return
	}
	_ = s.reconcileProxmoxDuplicates(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) reconcileProxmoxDuplicates(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `SELECT p.id,p.metadata FROM resources p WHERE p.type IN ('virtual_machine','lxc') AND p.status <> 'removed' AND p.metadata->>'proxmox'='true'`)
	if err != nil {
		return err
	}
	type guest struct {
		id   string
		meta map[string]any
	}
	var guests []guest
	for rows.Next() {
		var id string
		var raw []byte
		if rows.Scan(&id, &raw) != nil {
			continue
		}
		meta := map[string]any{}
		_ = json.Unmarshal(raw, &meta)
		guests = append(guests, guest{id: id, meta: meta})
	}
	rows.Close()

	for _, g := range guests {
		var hasAgent bool
		_ = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agents WHERE resource_id=$1)", g.id).Scan(&hasAgent)
		if hasAgent {
			continue
		}
		agentID, err := findAgentMatchForGuest(ctx, tx, g.id, asString(g.meta["dmi_uuid"]), anyStrings(g.meta["macs"]))
		if err != nil {
			return err
		}
		if agentID == "" || agentID == g.id {
			continue
		}
		if err := mergeProxmoxGuestIntoAgent(ctx, tx, g.id, agentID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func findAgentMatchForGuest(ctx context.Context, tx pgx.Tx, guestID, dmiUUID string, macs []string) (string, error) {
	dmiUUID = normalizeIdentity(dmiUUID)
	macSet := map[string]bool{}
	for _, value := range macs {
		if mac := normalizeMAC(value); mac != "" {
			macSet[mac] = true
		}
	}
	rows, err := tx.Query(ctx, `SELECT r.id,r.metadata FROM resources r JOIN agents a ON a.resource_id=r.id WHERE r.id<>$1 AND r.status<>'removed'`, guestID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	bestID, bestScore, ties := "", 0, 0
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
		for _, value := range anyStrings(meta["macs"]) {
			if macSet[normalizeMAC(value)] && score < 90 {
				score = 90
			}
		}
		if score > bestScore {
			bestID, bestScore, ties = id, score, 1
		} else if score != 0 && score == bestScore {
			ties++
		}
	}
	if bestScore < 90 || ties != 1 {
		return "", nil
	}
	return bestID, nil
}

func mergeProxmoxGuestIntoAgent(ctx context.Context, tx pgx.Tx, proxmoxID, agentID string) error {
	var pType, pName, pStatus string
	var pRaw, aRaw []byte
	var managed bool
	if err := tx.QueryRow(ctx, "SELECT type,name,status,metadata FROM resources WHERE id=$1", proxmoxID).Scan(&pType, &pName, &pStatus, &pRaw); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, "SELECT managed,metadata FROM resources WHERE id=$1", agentID).Scan(&managed, &aRaw); err != nil {
		return err
	}
	pMeta, aMeta := map[string]any{}, map[string]any{}
	_ = json.Unmarshal(pRaw, &pMeta)
	_ = json.Unmarshal(aRaw, &aMeta)
	for key, value := range pMeta {
		aMeta[key] = value
	}
	aMeta["agent_connected"] = true
	merged, _ := json.Marshal(aMeta)
	_, err := tx.Exec(ctx, "UPDATE resources SET type=$2,name=$3,status=$4,managed=$5,metadata=$6,updated_at=now() WHERE id=$1", agentID, pType, pName, pStatus, managed, merged)
	if err != nil {
		return err
	}

	_, _ = tx.Exec(ctx, `INSERT INTO resource_identities(id,resource_id,source,kind,value) SELECT id||'_m',$2,source,kind,value FROM resource_identities WHERE resource_id=$1 ON CONFLICT(source,kind,value) DO UPDATE SET resource_id=excluded.resource_id`, proxmoxID, agentID)
	_, _ = tx.Exec(ctx, "DELETE FROM resource_identities WHERE resource_id=$1", proxmoxID)
	_, _ = tx.Exec(ctx, `INSERT INTO relationships(id,source_id,target_id,kind) SELECT id||'_m',$2,target_id,kind FROM relationships WHERE source_id=$1 AND target_id<>$2 ON CONFLICT(source_id,target_id,kind) DO NOTHING`, proxmoxID, agentID)
	_, _ = tx.Exec(ctx, `INSERT INTO relationships(id,source_id,target_id,kind) SELECT id||'_m',source_id,$2,kind FROM relationships WHERE target_id=$1 AND source_id<>$2 ON CONFLICT(source_id,target_id,kind) DO NOTHING`, proxmoxID, agentID)
	_, _ = tx.Exec(ctx, "DELETE FROM relationships WHERE source_id=$1 OR target_id=$1", proxmoxID)
	_, _ = tx.Exec(ctx, "UPDATE resources SET metadata=jsonb_set(metadata,'{host_resource_id}',to_jsonb($2::text),true),updated_at=now() WHERE metadata->>'host_resource_id'=$1", proxmoxID, agentID)
	_, _ = tx.Exec(ctx, "DELETE FROM metrics_latest WHERE resource_id=$1", proxmoxID)
	_, _ = tx.Exec(ctx, "DELETE FROM resources WHERE id=$1", proxmoxID)
	_, _ = tx.Exec(ctx, "INSERT INTO activity(id,actor,action,resource_id,detail) VALUES($1,'system:v0.4','identity_reconciled',$2,jsonb_build_object('merged_resource',$3,'method','dmi_or_mac'))", newID("act"), agentID, proxmoxID)
	return nil
}

var _ = errors.Is
var _ = pgx.ErrNoRows
