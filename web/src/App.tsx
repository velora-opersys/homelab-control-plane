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

const nav = ['Home', 'Topology', 'Infrastructure', 'Applications', 'Backups', 'Automation', 'Remote Access', 'Activity', 'Documentation', 'Settings']

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
  return <div className="auth-wrap">
    <div className="auth-card">
      <div className="brand-mark">H</div>
      <p className="eyebrow">HOMELAB CONTROL PLANE</p>
      <h1>{mode === 'setup' ? 'Create your owner account' : 'Welcome back'}</h1>
      <p className="muted">{mode === 'setup' ? 'This account controls enrolment and privileged actions.' : 'Sign in to your control plane.'}</p>
      <form onSubmit={submit}>
        <label>Username<input autoFocus value={username} onChange={e=>setUsername(e.target.value)} /></label>
        <label>Password<input type="password" value={password} onChange={e=>setPassword(e.target.value)} /></label>
        {error && <div className="error">{error}</div>}
        <button className="primary" disabled={busy}>{busy ? 'Working…' : mode === 'setup' ? 'Create owner' : 'Sign in'}</button>
      </form>
    </div>
  </div>
}

function Shell({page,setPage,onLogout}:{page:string;setPage:(p:string)=>void;onLogout:()=>void}) {
  return <div className="layout">
    <aside>
      <div className="brand"><div className="brand-mark small">H</div><div><strong>Control Plane</strong><span>v0.1 dev</span></div></div>
      <nav>{nav.map(item => <button key={item} className={page===item?'active':''} onClick={()=>setPage(item)}>{item}</button>)}</nav>
      <div className="aside-footer"><button onClick={onLogout}>Sign out</button></div>
    </aside>
    <main>
      {page === 'Home' && <Home />}
      {page === 'Infrastructure' && <Infrastructure />}
      {page === 'Topology' && <TopologyPage />}
      {page === 'Settings' && <Settings />}
      {!['Home','Infrastructure','Topology','Settings'].includes(page) && <ComingSoon title={page} />}
    </main>
  </div>
}

function Header({title,subtitle,actions}:{title:string;subtitle:string;actions?:React.ReactNode}) {
  return <header className="page-header"><div><p className="eyebrow">CONTROL PLANE</p><h1>{title}</h1><p className="muted">{subtitle}</p></div>{actions}</header>
}

function Home() {
  const [data,setData] = useState<Dashboard|null>(null)
  const [resources,setResources] = useState<Resource[]>([])
  const [error,setError] = useState('')
  useEffect(()=>{ const load=()=>Promise.all([api<Dashboard>('/api/v1/dashboard'),api<Resource[]>('/api/v1/resources')]).then(([d,r])=>{setData(d);setResources(r)}).catch(e=>setError(String(e))); load(); const id=setInterval(load,15000); return()=>clearInterval(id)},[])
  if (error) return <ErrorState error={error}/>
  return <><Header title="Homelab health" subtitle="One view across everything the control plane knows about." />
    <section className="hero-health">
      <div><span className="health-number">{data?.health ?? '—'}<small>%</small></span><p>Overall health</p></div>
      <div className="health-copy"><strong>{(data?.warnings ?? 0) === 0 ? 'Everything looks healthy' : `${data?.warnings} item${data?.warnings===1?'':'s'} need attention`}</strong><span>{data?.online_agents ?? 0} agents online · {data?.resources ?? 0} known resources</span></div>
    </section>
    <div className="cards four">
      <Stat label="Hosts" value={data?.counts?.physical_host ?? 0} sub={`${data?.online_agents ?? 0} agent-connected`} />
      <Stat label="Applications" value={data?.counts?.application ?? 0} sub="discovery expands in v0.2" />
      <Stat label="Containers" value={data?.counts?.container ?? 0} sub="Docker inventory next" />
      <Stat label="Warnings" value={data?.warnings ?? 0} sub="disk ≥85% or memory ≥90%" />
    </div>
    <section className="section"><div className="section-title"><h2>Needs attention</h2><span>{resources.filter(r => (r.metrics?.disk_percent ?? 0)>=85 || (r.metrics?.memory_percent ?? 0)>=90 || r.status==='offline').length} items</span></div>
      <div className="attention-list">{resources.filter(r => (r.metrics?.disk_percent ?? 0)>=85 || (r.metrics?.memory_percent ?? 0)>=90 || r.status==='offline').slice(0,6).map(r=><div className="attention" key={r.id}><StatusDot status={r.status}/><div><strong>{r.name}</strong><span>{r.status==='offline'?'Agent is offline':(r.metrics?.disk_percent ?? 0)>=85?`Disk usage ${fmt(r.metrics?.disk_percent)}%`:`Memory usage ${fmt(r.metrics?.memory_percent)}%`}</span></div></div>)}{resources.length>0 && resources.every(r => (r.metrics?.disk_percent ?? 0)<85 && (r.metrics?.memory_percent ?? 0)<90 && r.status!=='offline') && <div className="empty">No current warnings.</div>}{resources.length===0 && <div className="empty">No hosts enrolled yet. Generate an enrolment token in Settings.</div>}</div>
    </section>
    <section className="section"><div className="section-title"><h2>Infrastructure pulse</h2><span>refreshes every 15s</span></div><ResourceTable resources={resources.slice(0,8)} /></section>
  </>
}

function Infrastructure() {
  const [resources,setResources]=useState<Resource[]>([])
  const [selected,setSelected]=useState<Resource|null>(null)
  useEffect(()=>{api<Resource[]>('/api/v1/resources').then(setResources)},[])
  return <><Header title="Infrastructure" subtitle="Discovered and enrolled hosts, virtual machines, services and devices." />
    <div className="split"><div><ResourceTable resources={resources} onSelect={setSelected}/></div><div>{selected?<ResourcePanel resource={selected}/>:<div className="panel empty-panel">Select a resource to inspect it.</div>}</div></div>
  </>
}

function ResourceTable({resources,onSelect}:{resources:Resource[];onSelect?:(r:Resource)=>void}) {
  return <div className="table"><div className="tr th"><span>Name</span><span>Status</span><span>CPU</span><span>Memory</span><span>Disk</span><span>Source</span></div>{resources.map(r=><button className="tr" key={r.id} onClick={()=>onSelect?.(r)}><span><strong>{r.name}</strong><small>{String(r.metadata?.os ?? r.type)} · {String(r.metadata?.arch ?? '')}</small></span><span><StatusDot status={r.status}/>{r.status}</span><span>{pct(r.metrics?.cpu_percent)}</span><span>{pct(r.metrics?.memory_percent)}</span><span>{pct(r.metrics?.disk_percent)}</span><span>{r.source}</span></button>)}{resources.length===0&&<div className="empty">No resources yet.</div>}</div>
}

function ResourcePanel({resource:r}:{resource:Resource}) {
  const caps = (r.metadata?.capabilities ?? {}) as Record<string,unknown>
  return <div className="panel resource-panel"><div className="resource-heading"><div><StatusDot status={r.status}/><h2>{r.name}</h2></div><span className={r.managed?'pill managed':'pill'}>{r.managed?'Managed':'Unmanaged'}</span></div><p className="muted">{String(r.metadata?.os ?? r.type)} · {String(r.metadata?.arch ?? '')}</p><div className="metric-grid"><Metric label="CPU" value={pct(r.metrics?.cpu_percent)}/><Metric label="Memory" value={pct(r.metrics?.memory_percent)}/><Metric label="Disk" value={pct(r.metrics?.disk_percent)}/><Metric label="Load" value={fmt(r.metrics?.load1)}/></div><h3>Capabilities</h3><div className="chips">{Object.entries(caps).map(([k,v])=><span key={k} className={v?'chip on':'chip'}>{k}</span>)}</div><h3>Identity</h3><dl><dt>Resource ID</dt><dd>{r.id}</dd><dt>Machine ID</dt><dd>{String(r.metadata?.machine_id ?? '—')}</dd><dt>IPs</dt><dd>{Array.isArray(r.metadata?.ips)?r.metadata.ips.join(', '):'—'}</dd><dt>Last seen</dt><dd>{r.last_seen?new Date(r.last_seen).toLocaleString():'—'}</dd></dl></div>
}

function TopologyPage() {
  const [graph,setGraph]=useState<Topology>({nodes:[],edges:[]})
  const [zoom,setZoom]=useState(1)
  const [selected,setSelected]=useState<string|null>(null)
  useEffect(()=>{api<Topology>('/api/v1/topology').then(setGraph)},[])
  const positions=useMemo(()=>Object.fromEntries(graph.nodes.map((n,i)=>[n.id,{x:120+(i%4)*230,y:100+Math.floor(i/4)*170}])),[graph])
  return <><Header title="Topology" subtitle="The live resource graph behind your homelab." actions={<div className="zoom"><button onClick={()=>setZoom(z=>Math.max(.5,z-.1))}>−</button><span>{Math.round(zoom*100)}%</span><button onClick={()=>setZoom(z=>Math.min(2,z+.1))}>+</button></div>}/>
    <div className="topology-frame"><svg viewBox="0 0 1000 650" style={{transform:`scale(${zoom})`,transformOrigin:'0 0'}}>{graph.edges.map(e=>{const a=positions[e.source],b=positions[e.target];if(!a||!b)return null;return <g key={e.id}><line x1={a.x+75} y1={a.y+35} x2={b.x+75} y2={b.y+35}/><text x={(a.x+b.x)/2+75} y={(a.y+b.y)/2+25}>{e.kind}</text></g>})}{graph.nodes.map(n=>{const p=positions[n.id];return <g key={n.id} className={selected===n.id?'node selected':'node'} onClick={()=>setSelected(n.id)}><rect x={p.x} y={p.y} width="150" height="72" rx="14"/><circle cx={p.x+20} cy={p.y+20} r="5" className={n.status==='online'?'online':'offline'}/><text x={p.x+34} y={p.y+24} className="node-name">{n.name.slice(0,18)}</text><text x={p.x+18} y={p.y+50} className="node-type">{n.type.replaceAll('_',' ')}</text></g>})}</svg>{graph.nodes.length===0&&<div className="topology-empty">Enrol a host and it will appear here automatically.</div>}</div>
  </>
}

function Settings() {
  const [token,setToken]=useState('')
  const [expires,setExpires]=useState('')
  const [error,setError]=useState('')
  async function generate(){setError('');try{const r=await api<{token:string;expires_at:string}>('/api/v1/enrolment-tokens',{method:'POST'});setToken(r.token);setExpires(r.expires_at)}catch(e){setError(String(e))}}
  return <><Header title="Settings" subtitle="Control-plane configuration and device enrolment."/><section className="section panel"><div className="section-title"><div><h2>Enrol a host</h2><p className="muted">Generate a single-use token valid for 20 minutes.</p></div><button className="primary compact" onClick={generate}>Generate token</button></div>{error&&<div className="error">{error}</div>}{token&&<div className="token-box"><label>Enrolment token</label><code>{token}</code><small>Expires {new Date(expires).toLocaleString()}</small><p>Build/run the agent with:</p><pre>homelab-agent --server {window.location.origin} --token {token}</pre></div>}</section></>
}

function ComingSoon({title}:{title:string}) { return <><Header title={title} subtitle="The foundation is ready for this module."/><div className="coming"><div className="brand-mark">H</div><h2>{title} is on the roadmap</h2><p>v0.1 is intentionally proving identity, enrolment, telemetry and the resource graph first.</p></div></> }
function Stat({label,value,sub}:{label:string;value:number|string;sub:string}){return <div className="stat card"><span>{label}</span><strong>{value}</strong><small>{sub}</small></div>}
function Metric({label,value}:{label:string;value:string}){return <div><span>{label}</span><strong>{value}</strong></div>}
function StatusDot({status}:{status:string}){return <i className={`status-dot ${status==='online'?'ok':status==='offline'?'bad':'warn'}`}/>} 
function ErrorState({error}:{error:string}){return <div className="error big">{error}</div>}
function fmt(v?:number){return typeof v==='number'?v.toFixed(1):'—'}
function pct(v?:number){return typeof v==='number'?`${v.toFixed(0)}%`:'—'}

export default App
