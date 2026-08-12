import './v02.css'
import { FormEvent, useEffect, useMemo, useState } from 'react'

type Resource = {
  id: string
  type: string
  name: string
  status: string
  managed: boolean
  source: string
  metadata: Record<string, unknown>
  metrics?: Record<string, number>
  last_seen?: string
}

type Dashboard = {
  health: number
  resources: number
  online_agents: number
  warnings: number
  counts: Record<string, number>
}

type Topology = {
  nodes: Array<{id:string; type:string; name:string; status:string; managed:boolean}>
  edges: Array<{id:string; source:string; target:string; kind:string}>
}

type CommandResult = {
  id: string
  kind: string
  status: 'queued'|'running'|'succeeded'|'failed'
  result: string
  error: string
}

const nav = ['Home', 'Topology', 'Infrastructure', 'Docker', 'Applications', 'Backups', 'Automation', 'Remote Access', 'Activity', 'Documentation', 'Settings']

async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const token = localStorage.getItem('hcp_token')
  const headers = new Headers(options.headers)
  headers.set('Content-Type', 'application/json')
  if (token) headers.set('Authorization', `Bearer ${token}`)
  const res = await fetch(path, {...options, headers})
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`)
  return body as T
}

function App() {
  const [ready, setReady] = useState(false)
  const [needsSetup, setNeedsSetup] = useState(false)
  const [authed, setAuthed] = useState(Boolean(localStorage.getItem('hcp_token')))
  const [page, setPage] = useState('Home')

  useEffect(() => {
    api<{needs_setup:boolean}>('/api/v1/setup/status')
      .then(r => setNeedsSetup(r.needs_setup))
      .finally(() => setReady(true))
  }, [])

  if (!ready) return <div className="center-screen"><div className="spinner" /></div>
  if (needsSetup) return <AuthCard mode="setup" onSuccess={() => { setNeedsSetup(false); setAuthed(true) }} />
  if (!authed) return <AuthCard mode="login" onSuccess={() => setAuthed(true)} />
  return <Shell page={page} setPage={setPage} onLogout={() => { localStorage.removeItem('hcp_token'); setAuthed(false) }} />
}

function AuthCard({mode, onSuccess}:{mode:'setup'|'login'; onSuccess:()=>void}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(e: FormEvent) {
    e.preventDefault(); setBusy(true); setError('')
    try {
      const path = mode === 'setup' ? '/api/v1/setup/owner' : '/api/v1/auth/login'
      const out = await api<{token:string}>(path, {method:'POST', body:JSON.stringify({username,password})})
      localStorage.setItem('hcp_token', out.token)
      onSuccess()
    } catch (e) { setError(e instanceof Error ? e.message : 'Request failed') }
    finally { setBusy(false) }
  }
  return <div className="auth-wrap"><div className="auth-card"><div className="brand-mark">A</div><p className="eyebrow">AXIOM</p><h1>{mode === 'setup' ? 'Create your owner account' : 'Welcome back'}</h1><p className="muted">{mode === 'setup' ? 'This account controls enrolment and privileged actions.' : 'Sign in to your homelab control plane.'}</p><form onSubmit={submit}><label>Username<input autoFocus value={username} onChange={e=>setUsername(e.target.value)} /></label><label>Password<input type="password" value={password} onChange={e=>setPassword(e.target.value)} /></label>{error && <div className="error">{error}</div>}<button className="primary" disabled={busy}>{busy ? 'Working…' : mode === 'setup' ? 'Create owner' : 'Sign in'}</button></form></div></div>
}

function Shell({page,setPage,onLogout}:{page:string;setPage:(p:string)=>void;onLogout:()=>void}) {
  return <div className="layout"><aside><div className="brand"><div className="brand-mark small">A</div><div><strong>Axiom</strong><span>v0.2 dev</span></div></div><nav>{nav.map(item => <button key={item} className={page===item?'active':''} onClick={()=>setPage(item)}>{item}</button>)}</nav><div className="aside-footer"><button onClick={onLogout}>Sign out</button></div></aside><main>{page === 'Home' && <Home />}{page === 'Infrastructure' && <Infrastructure />}{page === 'Topology' && <TopologyPage />}{page === 'Docker' && <DockerPage />}{page === 'Settings' && <Settings />}{!['Home','Infrastructure','Topology','Docker','Settings'].includes(page) && <ComingSoon title={page} />}</main></div>
}

function Header({title,subtitle,actions}:{title:string;subtitle:string;actions?:React.ReactNode}) {
  return <header className="page-header"><div><p className="eyebrow">AXIOM CONTROL PLANE</p><h1>{title}</h1><p className="muted">{subtitle}</p></div>{actions}</header>
}

function Home() {
  const [data,setData] = useState<Dashboard|null>(null)
  const [resources,setResources] = useState<Resource[]>([])
  const [error,setError] = useState('')
  useEffect(()=>{ const load=()=>Promise.all([api<Dashboard>('/api/v1/dashboard'),api<Resource[]>('/api/v1/resources')]).then(([d,r])=>{setData(d);setResources(r)}).catch(e=>setError(String(e))); load(); const id=setInterval(load,15000); return()=>clearInterval(id)},[])
  if (error) return <ErrorState error={error}/>
  const attention = resources.filter(r => (r.metrics?.disk_percent ?? 0)>=85 || (r.metrics?.memory_percent ?? 0)>=90 || r.status==='offline')
  return <><Header title="Homelab health" subtitle="One view across everything Axiom knows about." /><section className="hero-health"><div><span className="health-number">{data?.health ?? '—'}<small>%</small></span><p>Overall health</p></div><div className="health-copy"><strong>{(data?.warnings ?? 0) === 0 ? 'Everything looks healthy' : `${data?.warnings} item${data?.warnings===1?'':'s'} need attention`}</strong><span>{data?.online_agents ?? 0} agents online · {data?.resources ?? 0} known resources</span></div></section><div className="cards four"><Stat label="Hosts" value={data?.counts?.physical_host ?? 0} sub={`${data?.online_agents ?? 0} agent-connected`} /><Stat label="Applications" value={data?.counts?.application ?? 0} sub="Compose projects discovered" /><Stat label="Containers" value={data?.counts?.container ?? 0} sub={`${data?.counts?.docker_host ?? 0} Docker engines`} /><Stat label="Warnings" value={data?.warnings ?? 0} sub="disk ≥85% or memory ≥90%" /></div><section className="section"><div className="section-title"><h2>Needs attention</h2><span>{attention.length} items</span></div><div className="attention-list">{attention.slice(0,6).map(r=><div className="attention" key={r.id}><StatusDot status={r.status}/><div><strong>{r.name}</strong><span>{r.status==='offline'?'Agent is offline':(r.metrics?.disk_percent ?? 0)>=85?`Disk usage ${fmt(r.metrics?.disk_percent)}%`:`Memory usage ${fmt(r.metrics?.memory_percent)}%`}</span></div></div>)}{resources.length>0 && attention.length===0 && <div className="empty">No current warnings.</div>}{resources.length===0 && <div className="empty">No hosts enrolled yet. Generate a Linux install command in Settings.</div>}</div></section><section className="section"><div className="section-title"><h2>Infrastructure pulse</h2><span>refreshes every 15s</span></div><ResourceTable resources={resources.filter(r=>r.type==='physical_host').slice(0,8)} /></section></>
}

function Infrastructure() {
  const [resources,setResources]=useState<Resource[]>([])
  const [selected,setSelected]=useState<Resource|null>(null)
  async function load(selectID?:string){const next=await api<Resource[]>('/api/v1/resources');setResources(next);if(selectID)setSelected(next.find(r=>r.id===selectID)??null)}
  useEffect(()=>{load()},[])
  return <><Header title="Infrastructure" subtitle="Discovered and enrolled hosts, virtual machines, services and devices." /><div className="split"><div><ResourceTable resources={resources.filter(r=>r.type==='physical_host')} onSelect={setSelected}/></div><div>{selected?<ResourcePanel resource={selected} onChanged={()=>load(selected.id)}/>:<div className="panel empty-panel">Select a host to inspect it.</div>}</div></div></>
}

function ResourceTable({resources,onSelect}:{resources:Resource[];onSelect?:(r:Resource)=>void}) {
  return <div className="table"><div className="tr th"><span>Name</span><span>Status</span><span>CPU</span><span>Memory</span><span>Disk</span><span>Source</span></div>{resources.map(r=><button className="tr" key={r.id} onClick={()=>onSelect?.(r)}><span><strong>{r.name}</strong><small>{String(r.metadata?.os ?? r.type)} · {String(r.metadata?.arch ?? '')}</small></span><span><StatusDot status={r.status}/>{r.status}</span><span>{pct(r.metrics?.cpu_percent)}</span><span>{pct(r.metrics?.memory_percent)}</span><span>{pct(r.metrics?.disk_percent)}</span><span>{r.source}</span></button>)}{resources.length===0&&<div className="empty">No resources yet.</div>}</div>
}

function ResourcePanel({resource:r,onChanged}:{resource:Resource;onChanged:()=>void}) {
  const caps = (r.metadata?.capabilities ?? {}) as Record<string,unknown>
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  async function changeManaged(){setBusy(true);setError('');try{await api(`/api/v1/resources/${r.id}/managed`,{method:'POST',body:JSON.stringify({managed:!r.managed})});onChanged()}catch(e){setError(e instanceof Error?e.message:String(e))}finally{setBusy(false)}}
  return <div className="panel resource-panel"><div className="resource-heading"><div><StatusDot status={r.status}/><h2>{r.name}</h2></div><span className={r.managed?'pill managed':'pill'}>{r.managed?'Managed':'Unmanaged'}</span></div><p className="muted">{String(r.metadata?.os ?? r.type)} · {String(r.metadata?.arch ?? '')}</p><div className="metric-grid"><Metric label="CPU" value={pct(r.metrics?.cpu_percent)}/><Metric label="Memory" value={pct(r.metrics?.memory_percent)}/><Metric label="Disk" value={pct(r.metrics?.disk_percent)}/><Metric label="Load" value={fmt(r.metrics?.load1)}/></div>{error&&<div className="error">{error}</div>}<button className="primary compact" disabled={busy} onClick={changeManaged}>{busy?'Saving…':r.managed?'Set unmanaged':'Enable management'}</button><p className="muted small-copy">Management must be enabled before Axiom can start, stop or restart workloads on this host.</p><h3>Capabilities</h3><div className="chips">{Object.entries(caps).map(([k,v])=><span key={k} className={v?'chip on':'chip'}>{k}</span>)}</div><h3>Identity</h3><dl><dt>Resource ID</dt><dd>{r.id}</dd><dt>Machine ID</dt><dd>{String(r.metadata?.machine_id ?? '—')}</dd><dt>IPs</dt><dd>{Array.isArray(r.metadata?.ips)?r.metadata.ips.join(', '):'—'}</dd><dt>Last seen</dt><dd>{r.last_seen?new Date(r.last_seen).toLocaleString():'—'}</dd></dl></div>
}

function DockerPage() {
  const [resources,setResources]=useState<Resource[]>([])
  const [selected,setSelected]=useState<Resource|null>(null)
  const [error,setError]=useState('')
  async function load(){try{const next=await api<Resource[]>('/api/v1/resources');setResources(next);setSelected(cur=>cur?next.find(r=>r.id===cur.id)??null:cur)}catch(e){setError(String(e))}}
  useEffect(()=>{load();const id=setInterval(load,10000);return()=>clearInterval(id)},[])
  const engines=resources.filter(r=>r.type==='docker_host')
  const apps=resources.filter(r=>r.type==='application')
  const containers=resources.filter(r=>r.type==='container')
  if(error)return <ErrorState error={error}/>
  return <><Header title="Docker Control Centre" subtitle="Live Docker inventory discovered through Axiom agents." /><div className="cards four"><Stat label="Engines" value={engines.length} sub="agent-discovered"/><Stat label="Applications" value={apps.length} sub="Compose projects"/><Stat label="Containers" value={containers.length} sub={`${containers.filter(c=>c.status==='online').length} running`}/><Stat label="Unmanaged" value={containers.filter(c=>!c.managed).length} sub="actions disabled"/></div>{engines.length===0&&<div className="panel empty-panel">No Docker engines discovered yet. Install the v0.2 agent on a Linux Docker host.</div>}{engines.length>0&&<div className="docker-layout"><div className="docker-list panel">{apps.length>0&&<><p className="eyebrow">COMPOSE APPLICATIONS</p>{apps.map(app=><div className="docker-app" key={app.id}><strong>{app.name}</strong><span>{containers.filter(c=>c.metadata?.compose_project===app.metadata?.compose_project).length} containers</span></div>)}</>}<p className="eyebrow docker-heading">CONTAINERS</p>{containers.map(c=><button className={selected?.id===c.id?'docker-item active':'docker-item'} key={c.id} onClick={()=>setSelected(c)}><span><StatusDot status={c.status}/><strong>{c.name}</strong></span><small>{String(c.metadata?.image??'')}</small><em>{c.status}</em></button>)}</div><div>{selected?<DockerContainerPanel resource={selected} onRefresh={load}/>:<div className="panel empty-panel">Select a container to inspect and control it.</div>}</div></div>}</>
}

function DockerContainerPanel({resource:r,onRefresh}:{resource:Resource;onRefresh:()=>void}) {
  const [busy,setBusy]=useState('')
  const [error,setError]=useState('')
  const [output,setOutput]=useState('')
  async function action(name:'start'|'stop'|'restart'|'logs'){
    setBusy(name);setError('');if(name!=='logs')setOutput('')
    try{
      const queued=await api<{command_id:string}>(`/api/v1/docker/${r.id}/action`,{method:'POST',body:JSON.stringify({action:name})})
      for(let i=0;i<40;i++){
        await new Promise(resolve=>setTimeout(resolve,750))
        const command=await api<CommandResult>(`/api/v1/commands/${queued.command_id}`)
        if(command.status==='succeeded'){if(name==='logs')setOutput(command.result||'No log output.');await onRefresh();return}
        if(command.status==='failed')throw new Error(command.error||command.result||'Agent command failed')
      }
      throw new Error('The agent did not complete the command in time.')
    }catch(e){setError(e instanceof Error?e.message:String(e))}finally{setBusy('')}
  }
  return <div className="panel resource-panel"><div className="resource-heading"><div><StatusDot status={r.status}/><h2>{r.name}</h2></div><span className={r.managed?'pill managed':'pill'}>{r.managed?'Managed':'Unmanaged'}</span></div><p className="muted">{String(r.metadata?.image??'Unknown image')}</p><div className="metric-grid"><Metric label="CPU" value={pct(r.metrics?.cpu_percent)}/><Metric label="Memory" value={pct(r.metrics?.memory_percent)}/><Metric label="State" value={String(r.metadata?.state??r.status)}/><Metric label="Compose" value={String(r.metadata?.compose_project??'Standalone')}/></div><dl><dt>Service</dt><dd>{String(r.metadata?.compose_service??'—')}</dd><dt>Ports</dt><dd>{String(r.metadata?.ports??'—')}</dd><dt>Container ID</dt><dd>{String(r.metadata?.container_id??'—')}</dd></dl>{!r.managed&&<div className="notice">This container is read-only because its host is Unmanaged. Enable management on the host in Infrastructure first.</div>}{error&&<div className="error">{error}</div>}<div className="action-row"><button disabled={!r.managed||!!busy} onClick={()=>action('start')}>{busy==='start'?'Starting…':'Start'}</button><button disabled={!r.managed||!!busy} onClick={()=>action('stop')}>{busy==='stop'?'Stopping…':'Stop'}</button><button disabled={!r.managed||!!busy} onClick={()=>action('restart')}>{busy==='restart'?'Restarting…':'Restart'}</button><button disabled={!r.managed||!!busy} onClick={()=>action('logs')}>{busy==='logs'?'Loading…':'Logs'}</button></div>{output&&<pre className="logs-output">{output}</pre>}</div>
}

function TopologyPage() {
  const [graph,setGraph]=useState<Topology>({nodes:[],edges:[]})
  const [zoom,setZoom]=useState(1)
  const [selected,setSelected]=useState<string|null>(null)
  useEffect(()=>{const load=()=>api<Topology>('/api/v1/topology').then(setGraph);load();const id=setInterval(load,10000);return()=>clearInterval(id)},[])
  const positions=useMemo(()=>Object.fromEntries(graph.nodes.map((n,i)=>[n.id,{x:120+(i%4)*230,y:100+Math.floor(i/4)*170}])),[graph])
  return <><Header title="Topology" subtitle="Hosts, Docker engines, applications and containers from the live resource graph." actions={<div className="zoom"><button onClick={()=>setZoom(z=>Math.max(.5,z-.1))}>−</button><span>{Math.round(zoom*100)}%</span><button onClick={()=>setZoom(z=>Math.min(2,z+.1))}>+</button></div>}/><div className="topology-frame"><svg viewBox="0 0 1000 650" style={{transform:`scale(${zoom})`,transformOrigin:'0 0'}}>{graph.edges.map(e=>{const a=positions[e.source],b=positions[e.target];if(!a||!b)return null;return <g key={e.id}><line x1={a.x+75} y1={a.y+35} x2={b.x+75} y2={b.y+35}/><text x={(a.x+b.x)/2+75} y={(a.y+b.y)/2+25}>{e.kind}</text></g>})}{graph.nodes.map(n=>{const p=positions[n.id];return <g key={n.id} className={selected===n.id?'node selected':'node'} onClick={()=>setSelected(n.id)}><rect x={p.x} y={p.y} width="150" height="72" rx="14"/><circle cx={p.x+20} cy={p.y+20} r="5" className={n.status==='online'?'online':'offline'}/><text x={p.x+34} y={p.y+24} className="node-name">{n.name.slice(0,18)}</text><text x={p.x+18} y={p.y+50} className="node-type">{n.type.replaceAll('_',' ')}</text></g>})}</svg>{graph.nodes.length===0&&<div className="topology-empty">Enrol a host and it will appear here automatically.</div>}</div></>
}

function Settings() {
  const [token,setToken]=useState('')
  const [expires,setExpires]=useState('')
  const [error,setError]=useState('')
  const [copied,setCopied]=useState(false)
  async function generate(){setError('');setCopied(false);try{const r=await api<{token:string;expires_at:string}>('/api/v1/enrolment-tokens',{method:'POST'});setToken(r.token);setExpires(r.expires_at)}catch(e){setError(String(e))}}
  const command=token?`curl -fsSL ${window.location.origin}/install-agent.sh | sudo bash -s -- --server ${window.location.origin} --token ${token}`:''
  async function copy(){await navigator.clipboard.writeText(command);setCopied(true);setTimeout(()=>setCopied(false),1800)}
  return <><Header title="Settings" subtitle="Control-plane configuration and device enrolment."/><section className="section panel"><div className="section-title"><div><h2>Install Linux agent</h2><p className="muted">Generate a single-use command valid for 20 minutes. Supports Linux amd64 and arm64.</p></div><button className="primary compact" onClick={generate}>Generate install command</button></div>{error&&<div className="error">{error}</div>}{token&&<div className="token-box"><label>Copy and run on the Linux device</label><pre>{command}</pre><div className="copy-row"><button className="primary compact" onClick={copy}>{copied?'Copied!':'Copy command'}</button><small>Token expires {new Date(expires).toLocaleString()}</small></div><div className="notice">The host will appear as <strong>Unmanaged</strong>. You can enable management from Infrastructure after reviewing it.</div></div>}</section><section className="section panel"><div className="section-title"><div><h2>Agent delivery</h2><p className="muted">Axiom serves its own agent binaries locally; enrolled machines do not need GitHub access.</p></div></div><dl className="settings-dl"><dt>Installer</dt><dd>{window.location.origin}/install-agent.sh</dd><dt>Architectures</dt><dd>Linux amd64 · Linux arm64</dd><dt>Service</dt><dd>axiom-agent.service</dd></dl></section></>
}

function ComingSoon({title}:{title:string}) { return <><Header title={title} subtitle="The Axiom foundation is ready for this module."/><div className="coming"><div className="brand-mark">A</div><h2>{title} is on the roadmap</h2><p>v0.2 focuses on agent delivery, Docker discovery, controls and the resource graph.</p></div></> }
function Stat({label,value,sub}:{label:string;value:number|string;sub:string}){return <div className="stat card"><span>{label}</span><strong>{value}</strong><small>{sub}</small></div>}
function Metric({label,value}:{label:string;value:string}){return <div><span>{label}</span><strong>{value}</strong></div>}
function StatusDot({status}:{status:string}){return <i className={`status-dot ${status==='online'?'ok':status==='offline'?'bad':'warn'}`}/>} 
function ErrorState({error}:{error:string}){return <div className="error big">{error}</div>}
function fmt(v?:number){return typeof v==='number'?v.toFixed(1):'—'}
function pct(v?:number){return typeof v==='number'?`${v.toFixed(0)}%`:'—'}

export default App
