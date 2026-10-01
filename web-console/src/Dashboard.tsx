import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";
import Nodes from "./Nodes";
import Monitor from "./Monitor";
import Models from "./Models";
import { Service, Deployment, Node } from "./types";

export default function Dashboard() {
  const [tab, setTab] = useState("monitor"); const [services, setServices] = useState<Service[]>([]); const [deployments, setDeployments] = useState<Deployment[]>([]); const [nodes, setNodes] = useState<Node[]>([]);
  const [error, setError] = useState(""); const [sync, setSync] = useState(""); const [query, setQuery] = useState("");
  const [tokenDialog, setTokenDialog] = useState(false); const [token, setToken] = useState(sessionStorage.getItem("platform-token") || ""); const refreshing = useRef(false);
  const refresh = useCallback(async () => {
    if (refreshing.current) return; refreshing.current = true;
    try { const [m, d, n] = await Promise.all([api<{ services: Service[] }>("/v1/monitor/services"), api<Deployment[]>("/v1/deployments"), api<Node[]>("/v1/resources")]); setServices(m.services); setDeployments(d); setNodes(n); setSync(new Date().toISOString()); setError(""); }
    catch (e) { setError(e instanceof ApiError && e.status === 401 ? "控制面需要访问令牌，请点击右上角“访问设置”。" : String(e instanceof Error ? e.message : e)); }
    finally { refreshing.current = false; }
  }, []);
  useEffect(() => { void refresh(); const timer = setInterval(() => void refresh(), 5000); return () => clearInterval(timer); }, [refresh]);
  const attention = services.filter(s => ["degraded", "failed", "offline"].includes(s.status)).length;
  return <div className="workspace"><aside className="sidebar"><div className="brand"><span className="brand-mark">∷</span><div>算力协作<span>COMPUTE TOGETHER</span></div></div><p className="nav-caption">工作空间</p><nav>{[["monitor", "◉", "服务监控"], ["models", "◇", "模型部署"], ["nodes", "▦", "算力节点"]].map(([id, icon, label]) => <button className={tab === id ? "nav-active" : ""} key={id} onClick={() => setTab(id)}><span>{icon}</span>{label}{id === "monitor" && attention > 0 && <em>{attention}</em>}</button>)}</nav><div className="sidebar-footer"><span className="live-dot" />分布式推理控制台<small>节点 · 模型 · 每一条连接</small></div></aside>
    <div className="workspace-main"><header className="topbar"><span>协作平台 <b>/ {tab === "monitor" ? "服务监控" : tab === "models" ? "模型部署" : "算力节点"}</b></span><div><span className={`sync-status ${error ? "sync-error" : ""}`}>{error ? "同步异常" : sync ? "实时同步 · 5s" : "正在连接"}</span><button onClick={() => setTokenDialog(true)}>访问设置</button><button onClick={() => void refresh()}>刷新</button></div></header><div className="page-content">{error && <div className="error-banner" role="alert">{error}</div>}
      {tab === "monitor" && <Monitor {...{ services, deployments, nodes, sync, query, setQuery }} />}
      {tab === "models" && <Models {...{ deployments, nodes, refresh, setError }} monitor={id => { setQuery(id); setTab("monitor"); }} />}
      {tab === "nodes" && <div className="node-page"><Nodes /></div>}
    </div></div>{tokenDialog && <div className="modal-backdrop"><form className="token-dialog" onSubmit={e => { e.preventDefault(); if (token) sessionStorage.setItem("platform-token", token); else sessionStorage.removeItem("platform-token"); setTokenDialog(false); void refresh(); }}><h2>控制面访问设置</h2><p>令牌仅保存在当前浏览器会话，需与控制面配置一致。</p><label>Bearer token<input type="password" value={token} onChange={e => setToken(e.target.value)} autoComplete="off" /></label><div className="form-actions"><button className="primary" type="submit">保存并连接</button><button type="button" onClick={() => setTokenDialog(false)}>取消</button></div></form></div>}
  </div>;
}
