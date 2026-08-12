package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const version = "0.2.0-dev"

type server struct{ db *pgxpool.Pool }

type resourceView struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Name     string         `json:"name"`
	Status   string         `json:"status"`
	Managed  bool           `json:"managed"`
	Source   string         `json:"source"`
	Metadata map[string]any `json:"metadata"`
	Metrics  map[string]any `json:"metrics,omitempty"`
	LastSeen *time.Time     `json:"last_seen,omitempty"`
}

type dockerContainer struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Image          string  `json:"image"`
	State          string  `json:"state"`
	Status         string  `json:"status"`
	Ports          string  `json:"ports"`
	ComposeProject string  `json:"compose_project"`
	ComposeService string  `json:"compose_service"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryPercent  float64 `json:"memory_percent"`
}

type userKey struct{}
type authUser struct{ ID, Username, Role string }

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://homelab:homelab@localhost:5432/homelab?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	for i := 0; i < 30; i++ {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		log.Printf("waiting for database: %v", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("database unavailable: %v", err)
	}
	if err := migrate(ctx, pool); err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	s := &server{db: pool}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/setup/status", s.setupStatus)
	mux.HandleFunc("POST /api/v1/setup/owner", s.setupOwner)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.Handle("GET /api/v1/dashboard", s.requireAuth(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /api/v1/resources", s.requireAuth(http.HandlerFunc(s.resources)))
	mux.Handle("GET /api/v1/topology", s.requireAuth(http.HandlerFunc(s.topology)))
	mux.Handle("POST /api/v1/enrolment-tokens", s.requireAuth(http.HandlerFunc(s.createEnrolmentToken)))
	mux.Handle("POST /api/v1/resources/{id}/managed", s.requireAuth(http.HandlerFunc(s.setManaged)))
	mux.Handle("POST /api/v1/docker/{id}/action", s.requireAuth(http.HandlerFunc(s.dockerAction)))
	mux.Handle("GET /api/v1/commands/{id}", s.requireAuth(http.HandlerFunc(s.commandStatus)))
	mux.HandleFunc("POST /api/v1/agent/enrol", s.agentEnrol)
	mux.HandleFunc("POST /api/v1/agent/heartbeat", s.agentHeartbeat)
	mux.HandleFunc("GET /api/v1/agent/commands", s.agentCommands)
	mux.HandleFunc("POST /api/v1/agent/commands/{id}/result", s.agentCommandResult)
	mux.HandleFunc("GET /install-agent.sh", s.installAgentScript)
	mux.HandleFunc("GET /downloads/axiom-agent/{file}", s.agentBinary)

	addr := env("LISTEN_ADDR", ":8080")
	log.Printf("Axiom API %s listening on %s", version, addr)
	log.Fatal(http.ListenAndServe(addr, requestLogger(mux)))
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY,username TEXT NOT NULL UNIQUE,password_hash BYTEA NOT NULL,role TEXT NOT NULL DEFAULT 'owner',created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,expires_at TIMESTAMPTZ NOT NULL,last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS resources (id TEXT PRIMARY KEY,type TEXT NOT NULL,name TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'unknown',managed BOOLEAN NOT NULL DEFAULT false,source TEXT NOT NULL DEFAULT 'manual',external_id TEXT,metadata JSONB NOT NULL DEFAULT '{}'::jsonb,created_at TIMESTAMPTZ NOT NULL DEFAULT now(),updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE UNIQUE INDEX IF NOT EXISTS resources_source_external_idx ON resources(source,external_id) WHERE external_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS agents (resource_id TEXT PRIMARY KEY REFERENCES resources(id) ON DELETE CASCADE,secret_hash TEXT NOT NULL,version TEXT NOT NULL DEFAULT '',last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS metrics_latest (resource_id TEXT PRIMARY KEY REFERENCES resources(id) ON DELETE CASCADE,cpu_percent DOUBLE PRECISION NOT NULL DEFAULT 0,memory_percent DOUBLE PRECISION NOT NULL DEFAULT 0,disk_percent DOUBLE PRECISION NOT NULL DEFAULT 0,load1 DOUBLE PRECISION NOT NULL DEFAULT 0,uptime_seconds BIGINT NOT NULL DEFAULT 0,updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS relationships (id TEXT PRIMARY KEY,source_id TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,target_id TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,kind TEXT NOT NULL,created_at TIMESTAMPTZ NOT NULL DEFAULT now(),UNIQUE(source_id,target_id,kind));
CREATE TABLE IF NOT EXISTS enrolment_tokens (token_hash TEXT PRIMARY KEY,created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,expires_at TIMESTAMPTZ NOT NULL,used_at TIMESTAMPTZ,created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS activity (id TEXT PRIMARY KEY,actor TEXT NOT NULL,action TEXT NOT NULL,resource_id TEXT,detail JSONB NOT NULL DEFAULT '{}'::jsonb,created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS agent_commands (id TEXT PRIMARY KEY,resource_id TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,kind TEXT NOT NULL,payload JSONB NOT NULL DEFAULT '{}'::jsonb,status TEXT NOT NULL DEFAULT 'queued',result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,created_at TIMESTAMPTZ NOT NULL DEFAULT now(),started_at TIMESTAMPTZ,finished_at TIMESTAMPTZ);
CREATE INDEX IF NOT EXISTS agent_commands_agent_status_idx ON agent_commands(resource_id,status,created_at);`
	_, err := db.Exec(ctx, schema)
	return err
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "database": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "database": true, "version": version})
}

func (s *server) setupStatus(w http.ResponseWriter, r *http.Request) {
	var count int
	if err := s.db.QueryRow(r.Context(), "SELECT count(*) FROM users").Scan(&count); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"needs_setup": count == 0})
}

func (s *server) setupOwner(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if len(in.Username) < 3 || len(in.Password) < 10 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "username must be at least 3 characters and password at least 10"})
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil { writeError(w, 500, err); return }
	defer tx.Rollback(r.Context())
	var count int
	if err := tx.QueryRow(r.Context(), "SELECT count(*) FROM users").Scan(&count); err != nil || count != 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "setup already completed"}); return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil { writeError(w, 500, err); return }
	uid := newID("usr")
	if _, err := tx.Exec(r.Context(), "INSERT INTO users(id,username,password_hash,role) VALUES($1,$2,$3,'owner')", uid, in.Username, hash); err != nil { writeError(w, 500, err); return }
	if err := tx.Commit(r.Context()); err != nil { writeError(w, 500, err); return }
	token, err := s.newSession(r.Context(), uid)
	if err != nil { writeError(w, 500, err); return }
	writeJSON(w, 201, map[string]any{"token": token, "user": map[string]any{"id": uid, "username": in.Username, "role": "owner"}})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if err := decodeJSON(r, &in); err != nil { writeError(w, 400, err); return }
	var id, username, role string
	var passwordHash []byte
	err := s.db.QueryRow(r.Context(), "SELECT id,username,password_hash,role FROM users WHERE lower(username)=lower($1)", strings.TrimSpace(in.Username)).Scan(&id, &username, &passwordHash, &role)
	if err != nil || bcrypt.CompareHashAndPassword(passwordHash, []byte(in.Password)) != nil { writeJSON(w, 401, map[string]any{"error": "invalid username or password"}); return }
	token, err := s.newSession(r.Context(), id)
	if err != nil { writeError(w, 500, err); return }
	writeJSON(w, 200, map[string]any{"token": token, "user": map[string]any{"id": id, "username": username, "role": role}})
}

func (s *server) newSession(ctx context.Context, userID string) (string, error) {
	token := newToken(32)
	_, err := s.db.Exec(ctx, "INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '30 days')", hashToken(token), userID)
	return token, err
}

func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r.Header.Get("Authorization"))
		if token == "" { writeJSON(w, 401, map[string]any{"error": "authentication required"}); return }
		var uid, username, role string
		err := s.db.QueryRow(r.Context(), `SELECT u.id,u.username,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND s.last_seen_at>now()-interval '24 hours'`, hashToken(token)).Scan(&uid, &username, &role)
		if err != nil { writeJSON(w, 401, map[string]any{"error": "session expired"}); return }
		_, _ = s.db.Exec(r.Context(), "UPDATE sessions SET last_seen_at=now() WHERE token_hash=$1", hashToken(token))
		ctx := context.WithValue(r.Context(), userKey{}, authUser{ID: uid, Username: username, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), "SELECT type,count(*) FROM resources WHERE status <> 'removed' GROUP BY type")
	if err != nil { writeError(w, 500, err); return }
	defer rows.Close()
	counts := map[string]int{}; total := 0
	for rows.Next() { var typ string; var n int; if rows.Scan(&typ, &n) == nil { counts[typ] = n; total += n } }
	var online, warnings int
	_ = s.db.QueryRow(r.Context(), "SELECT count(*) FROM agents WHERE last_seen_at>now()-interval '90 seconds'").Scan(&online)
	_ = s.db.QueryRow(r.Context(), "SELECT count(*) FROM metrics_latest WHERE disk_percent>=85 OR memory_percent>=90").Scan(&warnings)
	health := 100; if total > 0 && warnings > 0 { health = 100 - min(warnings*8, 60) }
	writeJSON(w, 200, map[string]any{"health": health, "resources": total, "online_agents": online, "warnings": warnings, "counts": counts})
}

func (s *server) resources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT r.id,r.type,r.name,CASE WHEN a.resource_id IS NOT NULL AND a.last_seen_at<now()-interval '90 seconds' THEN 'offline' ELSE r.status END,r.managed,r.source,r.metadata,m.cpu_percent,m.memory_percent,m.disk_percent,m.load1,m.uptime_seconds,a.last_seen_at FROM resources r LEFT JOIN agents a ON a.resource_id=r.id LEFT JOIN metrics_latest m ON m.resource_id=r.id WHERE r.status <> 'removed' ORDER BY r.type,r.name`)
	if err != nil { writeError(w, 500, err); return }
	defer rows.Close(); out := []resourceView{}
	for rows.Next() {
		var v resourceView; var metadata []byte; var cpu, mem, disk, load *float64; var uptime *int64
		if err := rows.Scan(&v.ID, &v.Type, &v.Name, &v.Status, &v.Managed, &v.Source, &metadata, &cpu, &mem, &disk, &load, &uptime, &v.LastSeen); err != nil { writeError(w, 500, err); return }
		_ = json.Unmarshal(metadata, &v.Metadata)
		if cpu != nil { v.Metrics = map[string]any{"cpu_percent": *cpu, "memory_percent": derefFloat(mem), "disk_percent": derefFloat(disk), "load1": derefFloat(load), "uptime_seconds": derefInt(uptime)} }
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

func (s *server) topology(w http.ResponseWriter, r *http.Request) {
	nodes := []map[string]any{}
	rows, err := s.db.Query(r.Context(), "SELECT id,type,name,status,managed FROM resources WHERE status <> 'removed' ORDER BY name")
	if err != nil { writeError(w, 500, err); return }
	for rows.Next() { var id, typ, name, status string; var managed bool; _ = rows.Scan(&id, &typ, &name, &status, &managed); nodes = append(nodes, map[string]any{"id": id, "type": typ, "name": name, "status": status, "managed": managed}) }
	rows.Close(); edges := []map[string]any{}
	erows, err := s.db.Query(r.Context(), `SELECT rel.id,rel.source_id,rel.target_id,rel.kind FROM relationships rel JOIN resources s ON s.id=rel.source_id JOIN resources t ON t.id=rel.target_id WHERE s.status <> 'removed' AND t.status <> 'removed'`)
	if err == nil { defer erows.Close(); for erows.Next() { var id, source, target, kind string; _ = erows.Scan(&id, &source, &target, &kind); edges = append(edges, map[string]any{"id": id, "source": source, "target": target, "kind": kind}) } }
	writeJSON(w, 200, map[string]any{"nodes": nodes, "edges": edges})
}

func (s *server) createEnrolmentToken(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey{}).(authUser); token := newToken(24); expires := time.Now().Add(20*time.Minute)
	if _, err := s.db.Exec(r.Context(), "INSERT INTO enrolment_tokens(token_hash,created_by,expires_at) VALUES($1,$2,$3)", hashToken(token), u.ID, expires); err != nil { writeError(w, 500, err); return }
	writeJSON(w, 201, map[string]any{"token": token, "expires_at": expires})
}

func (s *server) setManaged(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey{}).(authUser); id := r.PathValue("id")
	var in struct{ Managed bool `json:"managed"` }
	if err := decodeJSON(r, &in); err != nil { writeError(w, 400, err); return }
	tx, err := s.db.Begin(r.Context()); if err != nil { writeError(w, 500, err); return }; defer tx.Rollback(r.Context())
	var typ string
	if err := tx.QueryRow(r.Context(), "UPDATE resources SET managed=$2,updated_at=now() WHERE id=$1 RETURNING type", id, in.Managed).Scan(&typ); err != nil { writeJSON(w, 404, map[string]any{"error": "resource not found"}); return }
	if typ == "physical_host" { _, _ = tx.Exec(r.Context(), "UPDATE resources SET managed=$2,updated_at=now() WHERE metadata->>'host_resource_id'=$1", id, in.Managed) }
	_, _ = tx.Exec(r.Context(), "INSERT INTO activity(id,actor,action,resource_id,detail) VALUES($1,$2,'managed_state_changed',$3,jsonb_build_object('managed',$4))", newID("act"), "user:"+u.ID, id, in.Managed)
	if err := tx.Commit(r.Context()); err != nil { writeError(w, 500, err); return }
	writeJSON(w, 200, map[string]any{"id": id, "managed": in.Managed})
}

func (s *server) dockerAction(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey{}).(authUser); resourceID := r.PathValue("id")
	var in struct{ Action string `json:"action"` }
	if err := decodeJSON(r, &in); err != nil { writeError(w, 400, err); return }
	if !map[string]bool{"start": true, "stop": true, "restart": true, "logs": true}[in.Action] { writeJSON(w, 400, map[string]any{"error": "unsupported Docker action"}); return }
	var typ, hostID, containerID string; var managed bool
	err := s.db.QueryRow(r.Context(), "SELECT type,managed,metadata->>'host_resource_id',metadata->>'container_id' FROM resources WHERE id=$1", resourceID).Scan(&typ, &managed, &hostID, &containerID)
	if err != nil || typ != "container" { writeJSON(w, 404, map[string]any{"error": "container not found"}); return }
	if !managed { writeJSON(w, 403, map[string]any{"error": "container host is unmanaged; enable management before running actions"}); return }
	commandID := newID("cmd"); payload, _ := json.Marshal(map[string]any{"container_id": containerID, "container_resource_id": resourceID})
	if _, err = s.db.Exec(r.Context(), "INSERT INTO agent_commands(id,resource_id,kind,payload,created_by) VALUES($1,$2,$3,$4,$5)", commandID, hostID, "docker_"+in.Action, payload, u.ID); err != nil { writeError(w, 500, err); return }
	_, _ = s.db.Exec(r.Context(), "INSERT INTO activity(id,actor,action,resource_id,detail) VALUES($1,$2,$3,$4,jsonb_build_object('command_id',$5))", newID("act"), "user:"+u.ID, "docker_"+in.Action, resourceID, commandID)
	writeJSON(w, 202, map[string]any{"command_id": commandID, "status": "queued"})
}

func (s *server) commandStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id"); var status, result, commandError, kind string; var created time.Time; var started, finished *time.Time
	if err := s.db.QueryRow(r.Context(), "SELECT kind,status,result,error,created_at,started_at,finished_at FROM agent_commands WHERE id=$1", id).Scan(&kind, &status, &result, &commandError, &created, &started, &finished); err != nil { writeJSON(w, 404, map[string]any{"error": "command not found"}); return }
	writeJSON(w, 200, map[string]any{"id": id, "kind": kind, "status": status, "result": result, "error": commandError, "created_at": created, "started_at": started, "finished_at": finished})
}

func (s *server) agentEnrol(w http.ResponseWriter, r *http.Request) {
	var in struct { Token string `json:"token"`; Hostname string `json:"hostname"`; OS string `json:"os"`; Arch string `json:"arch"`; MachineID string `json:"machine_id"`; Version string `json:"version"`; IPs []string `json:"ips"`; MACs []string `json:"macs"`; Capabilities map[string]any `json:"capabilities"` }
	if err := decodeJSON(r, &in); err != nil { writeError(w, 400, err); return }
	if in.Token == "" || in.Hostname == "" || in.MachineID == "" { writeJSON(w, 400, map[string]any{"error": "token, hostname and machine_id are required"}); return }
	tx, err := s.db.Begin(r.Context()); if err != nil { writeError(w, 500, err); return }; defer tx.Rollback(r.Context())
	var createdBy string
	if err = tx.QueryRow(r.Context(), `UPDATE enrolment_tokens SET used_at=now() WHERE token_hash=$1 AND used_at IS NULL AND expires_at>now() RETURNING created_by`, hashToken(in.Token)).Scan(&createdBy); err != nil { writeJSON(w, 401, map[string]any{"error": "invalid, expired or already-used enrolment token"}); return }
	metadata, _ := json.Marshal(map[string]any{"os": in.OS, "arch": in.Arch, "machine_id": in.MachineID, "ips": in.IPs, "macs": in.MACs, "capabilities": in.Capabilities})
	var rid string; err = tx.QueryRow(r.Context(), "SELECT id FROM resources WHERE source='agent' AND external_id=$1", in.MachineID).Scan(&rid)
	if errors.Is(err, pgx.ErrNoRows) { rid = newID("res"); _, err = tx.Exec(r.Context(), `INSERT INTO resources(id,type,name,status,managed,source,external_id,metadata) VALUES($1,'physical_host',$2,'online',false,'agent',$3,$4)`, rid, in.Hostname, in.MachineID, metadata) } else if err == nil { _, err = tx.Exec(r.Context(), "UPDATE resources SET name=$2,status='online',metadata=$3,updated_at=now() WHERE id=$1", rid, in.Hostname, metadata) }
	if err != nil { writeError(w, 500, err); return }
	secret := newToken(32)
	if _, err = tx.Exec(r.Context(), `INSERT INTO agents(resource_id,secret_hash,version,last_seen_at) VALUES($1,$2,$3,now()) ON CONFLICT(resource_id) DO UPDATE SET secret_hash=excluded.secret_hash,version=excluded.version,last_seen_at=now()`, rid, hashToken(secret), in.Version); err != nil { writeError(w, 500, err); return }
	_, _ = tx.Exec(r.Context(), "INSERT INTO activity(id,actor,action,resource_id,detail) VALUES($1,$2,'agent_enrolled',$3,'{}')", newID("act"), "user:"+createdBy, rid)
	if err := tx.Commit(r.Context()); err != nil { writeError(w, 500, err); return }
	writeJSON(w, 201, map[string]any{"resource_id": rid, "agent_secret": secret})
}

func (s *server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	rid, ok := s.authenticateAgent(w, r); if !ok { return }
	var in struct { CPUPercent float64 `json:"cpu_percent"`; MemoryPercent float64 `json:"memory_percent"`; DiskPercent float64 `json:"disk_percent"`; Load1 float64 `json:"load1"`; UptimeSeconds int64 `json:"uptime_seconds"`; Version string `json:"version"`; Docker []dockerContainer `json:"docker"` }
	if err := decodeJSON(r, &in); err != nil { writeError(w, 400, err); return }
	tx, err := s.db.Begin(r.Context()); if err != nil { writeError(w, 500, err); return }; defer tx.Rollback(r.Context())
	_, _ = tx.Exec(r.Context(), "UPDATE agents SET last_seen_at=now(),version=$2 WHERE resource_id=$1", rid, in.Version)
	_, _ = tx.Exec(r.Context(), "UPDATE resources SET status='online',updated_at=now() WHERE id=$1", rid)
	if _, err = tx.Exec(r.Context(), `INSERT INTO metrics_latest(resource_id,cpu_percent,memory_percent,disk_percent,load1,uptime_seconds,updated_at) VALUES($1,$2,$3,$4,$5,$6,now()) ON CONFLICT(resource_id) DO UPDATE SET cpu_percent=excluded.cpu_percent,memory_percent=excluded.memory_percent,disk_percent=excluded.disk_percent,load1=excluded.load1,uptime_seconds=excluded.uptime_seconds,updated_at=now()`, rid, clamp(in.CPUPercent), clamp(in.MemoryPercent), clamp(in.DiskPercent), in.Load1, in.UptimeSeconds); err != nil { writeError(w, 500, err); return }
	if in.Docker != nil { if err := s.syncDocker(r.Context(), tx, rid, in.Docker); err != nil { writeError(w, 500, err); return } }
	if err := tx.Commit(r.Context()); err != nil { writeError(w, 500, err); return }
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) syncDocker(ctx context.Context, tx pgx.Tx, hostID string, containers []dockerContainer) error {
	var hostName string; var hostManaged bool
	if err := tx.QueryRow(ctx, "SELECT name,managed FROM resources WHERE id=$1", hostID).Scan(&hostName, &hostManaged); err != nil { return err }
	engineID, err := ensureResource(ctx, tx, "agent", hostID+":docker", "docker_host", hostName+" Docker", "online", hostManaged, map[string]any{"host_resource_id": hostID, "engine": "docker"}); if err != nil { return err }
	if err := ensureRelationship(ctx, tx, hostID, engineID, "HOSTS"); err != nil { return err }
	_, _ = tx.Exec(ctx, "UPDATE resources SET status='removed',updated_at=now() WHERE source='agent' AND type IN ('container','application') AND external_id LIKE $1", hostID+":docker-%")
	apps := map[string]string{}
	for _, c := range containers {
		if c.ID == "" { continue }
		status := "stopped"; if strings.EqualFold(c.State, "running") { status = "online" }
		meta := map[string]any{"host_resource_id": hostID, "docker_resource_id": engineID, "container_id": c.ID, "image": c.Image, "state": c.State, "docker_status": c.Status, "ports": c.Ports, "compose_project": c.ComposeProject, "compose_service": c.ComposeService}
		containerID, err := ensureResource(ctx, tx, "agent", hostID+":docker-container:"+c.ID, "container", c.Name, status, hostManaged, meta); if err != nil { return err }
		if _, err = tx.Exec(ctx, `INSERT INTO metrics_latest(resource_id,cpu_percent,memory_percent,disk_percent,load1,uptime_seconds,updated_at) VALUES($1,$2,$3,0,0,0,now()) ON CONFLICT(resource_id) DO UPDATE SET cpu_percent=excluded.cpu_percent,memory_percent=excluded.memory_percent,updated_at=now()`, containerID, clamp(c.CPUPercent), clamp(c.MemoryPercent)); err != nil { return err }
		if c.ComposeProject != "" {
			appID := apps[c.ComposeProject]
			if appID == "" { appID, err = ensureResource(ctx, tx, "agent", hostID+":docker-app:"+c.ComposeProject, "application", c.ComposeProject, "online", hostManaged, map[string]any{"host_resource_id": hostID, "docker_resource_id": engineID, "compose_project": c.ComposeProject}); if err != nil { return err }; apps[c.ComposeProject] = appID; if err := ensureRelationship(ctx, tx, engineID, appID, "RUNS"); err != nil { return err } }
			if err := ensureRelationship(ctx, tx, appID, containerID, "CONTAINS"); err != nil { return err }
		} else if err := ensureRelationship(ctx, tx, engineID, containerID, "RUNS"); err != nil { return err }
	}
	return nil
}

func ensureResource(ctx context.Context, tx pgx.Tx, source, externalID, typ, name, status string, managed bool, metadata map[string]any) (string, error) {
	metadataJSON, _ := json.Marshal(metadata); var id string
	err := tx.QueryRow(ctx, "SELECT id FROM resources WHERE source=$1 AND external_id=$2", source, externalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) { id = newID("res"); _, err = tx.Exec(ctx, "INSERT INTO resources(id,type,name,status,managed,source,external_id,metadata) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", id, typ, name, status, managed, source, externalID, metadataJSON); return id, err }
	if err != nil { return "", err }
	_, err = tx.Exec(ctx, "UPDATE resources SET type=$2,name=$3,status=$4,managed=$5,metadata=$6,updated_at=now() WHERE id=$1", id, typ, name, status, managed, metadataJSON)
	return id, err
}

func ensureRelationship(ctx context.Context, tx pgx.Tx, sourceID, targetID, kind string) error { _, err := tx.Exec(ctx, "INSERT INTO relationships(id,source_id,target_id,kind) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,target_id,kind) DO NOTHING", newID("rel"), sourceID, targetID, kind); return err }

func (s *server) agentCommands(w http.ResponseWriter, r *http.Request) {
	rid, ok := s.authenticateAgent(w, r); if !ok { return }
	tx, err := s.db.Begin(r.Context()); if err != nil { writeError(w, 500, err); return }; defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), `SELECT id,kind,payload FROM agent_commands WHERE resource_id=$1 AND status='queued' ORDER BY created_at LIMIT 5 FOR UPDATE SKIP LOCKED`, rid); if err != nil { writeError(w, 500, err); return }
	commands := []map[string]any{}; ids := []string{}
	for rows.Next() { var id, kind string; var payloadBytes []byte; if rows.Scan(&id, &kind, &payloadBytes) != nil { continue }; payload := map[string]any{}; _ = json.Unmarshal(payloadBytes, &payload); commands = append(commands, map[string]any{"id": id, "kind": kind, "container_id": payload["container_id"], "payload": payload}); ids = append(ids, id) }
	rows.Close(); for _, id := range ids { _, _ = tx.Exec(r.Context(), "UPDATE agent_commands SET status='running',started_at=now() WHERE id=$1", id) }
	if err := tx.Commit(r.Context()); err != nil { writeError(w, 500, err); return }
	writeJSON(w, 200, commands)
}

func (s *server) agentCommandResult(w http.ResponseWriter, r *http.Request) {
	rid, ok := s.authenticateAgent(w, r); if !ok { return }; id := r.PathValue("id")
	var in struct { Success bool `json:"success"`; Result string `json:"result"`; Error string `json:"error"` }
	if err := decodeJSON(r, &in); err != nil { writeError(w, 400, err); return }
	status := "failed"; if in.Success { status = "succeeded" }
	result, err := s.db.Exec(r.Context(), "UPDATE agent_commands SET status=$3,result=$4,error=$5,finished_at=now() WHERE id=$1 AND resource_id=$2", id, rid, status, limitString(in.Result, 128*1024), limitString(in.Error, 8*1024)); if err != nil { writeError(w, 500, err); return }
	if result.RowsAffected() == 0 { writeJSON(w, 404, map[string]any{"error": "command not found"}); return }
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) authenticateAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	rid := strings.TrimSpace(r.Header.Get("X-Agent-ID")); secret := bearer(r.Header.Get("Authorization"))
	if rid == "" || secret == "" { writeJSON(w, 401, map[string]any{"error": "agent authentication required"}); return "", false }
	var stored string
	if err := s.db.QueryRow(r.Context(), "SELECT secret_hash FROM agents WHERE resource_id=$1", rid).Scan(&stored); err != nil || stored != hashToken(secret) { writeJSON(w, 401, map[string]any{"error": "invalid agent credentials"}); return "", false }
	return rid, true
}

func (s *server) installAgentScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8"); w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, `#!/usr/bin/env bash
set -euo pipefail
SERVER=""; TOKEN=""
while [[ $# -gt 0 ]]; do case "$1" in --server) SERVER="${2:-}"; shift 2 ;; --token) TOKEN="${2:-}"; shift 2 ;; *) echo "Unknown option: $1" >&2; exit 2 ;; esac; done
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then echo "Run the Axiom agent installer with sudo/root." >&2; exit 1; fi
if [[ -z "$SERVER" || -z "$TOKEN" ]]; then echo "Usage: install-agent.sh --server http://AXIOM:8787 --token TOKEN" >&2; exit 2; fi
command -v curl >/dev/null 2>&1 || { echo "curl is required." >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "systemd is required." >&2; exit 1; }
case "$(uname -m)" in x86_64|amd64) ARCH="amd64" ;; aarch64|arm64) ARCH="arm64" ;; *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac
SERVER="${SERVER%/}"; INSTALL_DIR="/opt/axiom-agent"; STATE_DIR="/var/lib/axiom-agent"; BIN="$INSTALL_DIR/axiom-agent"; CONFIG="$STATE_DIR/agent.json"
mkdir -p "$INSTALL_DIR" "$STATE_DIR"; chmod 700 "$STATE_DIR"; systemctl stop axiom-agent.service >/dev/null 2>&1 || true
TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
curl -fsSL "$SERVER/downloads/axiom-agent/linux-$ARCH" -o "$TMP"; install -m 0755 "$TMP" "$BIN"
"$BIN" --server "$SERVER" --token "$TOKEN" --config "$CONFIG" --once
cat >/etc/systemd/system/axiom-agent.service <<UNIT
[Unit]
Description=Axiom Homelab Agent
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart=$BIN --config $CONFIG
Restart=always
RestartSec=5
NoNewPrivileges=true
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload; systemctl enable --now axiom-agent.service
echo; echo "Axiom agent installed successfully."; echo "Service: systemctl status axiom-agent"; echo "Logs: journalctl -u axiom-agent -f"
`)
}

func (s *server) agentBinary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file"); if name != "linux-amd64" && name != "linux-arm64" { http.NotFound(w, r); return }
	path := filepath.Join("/opt/axiom/agents", "axiom-agent-"+name); if _, err := os.Stat(path); err != nil { http.NotFound(w, r); return }
	w.Header().Set("Content-Type", "application/octet-stream"); w.Header().Set("Content-Disposition", `attachment; filename="axiom-agent"`); w.Header().Set("Cache-Control", "public, max-age=300"); http.ServeFile(w, r, path)
}

func requestLogger(next http.Handler) http.Handler { return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { start := time.Now(); next.ServeHTTP(w, r); log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond)) }) }
func decodeJSON(r *http.Request, dst any) error { defer r.Body.Close(); dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20)); dec.DisallowUnknownFields(); return dec.Decode(dst) }
func writeJSON(w http.ResponseWriter, status int, v any) { w.Header().Set("Content-Type", "application/json"); w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
func writeError(w http.ResponseWriter, status int, err error) { log.Printf("request error: %v", err); writeJSON(w, status, map[string]any{"error": http.StatusText(status)}) }
func newID(prefix string) string { return prefix + "_" + newToken(12) }
func newToken(n int) string { b := make([]byte, n); if _, err := rand.Read(b); err != nil { panic(err) }; return hex.EncodeToString(b) }
func hashToken(v string) string { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }
func bearer(v string) string { if strings.HasPrefix(v, "Bearer ") { return strings.TrimSpace(strings.TrimPrefix(v, "Bearer ")) }; return "" }
func env(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }
func clamp(v float64) float64 { if v < 0 { return 0 }; if v > 100 { return 100 }; return v }
func derefFloat(v *float64) float64 { if v == nil { return 0 }; return *v }
func derefInt(v *int64) int64 { if v == nil { return 0 }; return *v }
func min(a, b int) int { if a < b { return a }; return b }
func limitString(v string, n int) string { if len(v) <= n { return v }; return v[len(v)-n:] }
