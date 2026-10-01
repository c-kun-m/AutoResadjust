import { FormEvent, useCallback, useEffect, useState } from "react";
import { api } from "./api";
import { Node, time } from "./types";

type Group = { id: string; name: string; node_ids: string[]; max_rtt_ms: number; max_jitter_ms: number; min_mbps: number; max_age_seconds: number };
type Link = { source_id: string; target_id: string; status: string; rtt_p95_ms: number; jitter_ms: number; upload_mbps: number; observed_at: string; error?: string };
type Probe = { id: string; source_id: string; target_id: string; status: string; created_at: string };
const blank: Group = { id: "", name: "", node_ids: [], max_rtt_ms: 20, max_jitter_ms: 10, min_mbps: 100, max_age_seconds: 300 };
const statuses: Record<string, string> = { unknown: "尚未测量", stale: "观测过期", failed: "测量失败", measured: "已测量", slow: "不满足组策略", pending: "等待节点", running: "测量中", completed: "完成" };

export default function Network({ nodes }: { nodes: Node[] }) {
  const [groups, setGroups] = useState<Group[]>([]), [links, setLinks] = useState<Link[]>([]), [probes, setProbes] = useState<Probe[]>([]);
  const [selected, setSelected] = useState(""), [form, setForm] = useState<Group>(blank);
  const [source, setSource] = useState(""), [target, setTarget] = useState(""), [error, setError] = useState(""), [notice, setNotice] = useState(""), [busy, setBusy] = useState(false);
  const refresh = useCallback(async (signal?: AbortSignal) => {
    const [g, l, p] = await Promise.all([api<Group[]>("/v1/network-groups", { signal }), api<Link[]>(`/v1/network-links?group_id=${encodeURIComponent(selected)}`, { signal }), api<Probe[]>("/v1/network-probes", { signal })]);
    setGroups(g); setLinks(l); setProbes(p);
  }, [selected]);
  useEffect(() => {
    const controller = new AbortController(); let loading = false;
    const poll = async () => { if (loading) return; loading = true; try { await refresh(controller.signal); } catch (e) { if (!controller.signal.aborted) setError(String(e)); } finally { loading = false; } };
    void poll(); const timer = setInterval(() => void poll(), 5000);
    return () => { controller.abort(); clearInterval(timer); };
  }, [refresh]);
  const group = groups.find(g => g.id === selected);
  const members = group ? group.node_ids : nodes.slice(0, 8).map(n => n.id);
  const label = (id: string) => nodes.find(n => n.id === id)?.name || id;
  const linkStatus = (l?: Link) => {
    if (!l) return "unknown";
    if (l.status === "stale" || Date.now() - new Date(l.observed_at).getTime() > (group?.max_age_seconds ?? 300) * 1000) return "stale";
    if (l.status !== "measured") return l.status;
    if (group && (l.rtt_p95_ms > group.max_rtt_ms || l.jitter_ms > group.max_jitter_ms || l.upload_mbps < group.min_mbps)) return "slow";
    return "measured";
  };
  const measure = async (e: FormEvent) => {
    e.preventDefault(); setBusy(true); setError(""); setNotice("");
    try { await api("/v1/network-probes", { method: "POST", body: JSON.stringify({ source_id: source, target_id: target }) }); setNotice("探测已下发，节点将在下一次心跳领取任务。完成后交换方向，再测另一条链路。"); await refresh(); }
    catch (e) { setError(String(e)); } finally { setBusy(false); }
  };
  const save = async (e: FormEvent) => {
    e.preventDefault(); setBusy(true); setError(""); setNotice("");
    try { const g = await api<Group>("/v1/network-groups", { method: "POST", body: JSON.stringify(form) }); setSelected(g.id); setNotice("网络组已保存。模型部署时填写此组标识，调度会检查两方向的新鲜测量。"); await refresh(); }
    catch (e) { setError(String(e)); } finally { setBusy(false); }
  };
  return <>
    <div className="page-heading"><div><h1>网络与分组</h1><p>先测量，再决定哪些显卡适合一起计算。</p></div></div>
    {error && <div className="error-banner" role="alert">{error}</div>}{notice && <p role="status" className="panel-note">{notice}</p>}
    <section className="panel network-panel"><div className="panel-heading"><div><h2>有向链路矩阵</h2><p>行是发送节点，列是接收节点。两方向独立测量，选择网络组后可检查链路是否符合策略。</p></div><label>查看网络组<select value={selected} onChange={e => setSelected(e.target.value)}><option value="">节点概览（最多 8 台）</option>{groups.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}</select></label></div>
      {group && <p className="panel-note">{group.id} · RTT ≤{group.max_rtt_ms} ms · 抖动 ≤{group.max_jitter_ms} ms · 上传 ≥{group.min_mbps} Mbps · 有效期 {group.max_age_seconds}s <button onClick={() => setForm({ ...group, node_ids: [...group.node_ids] })}>编辑此组</button></p>}
      <div className="table-wrap"><table className="network-matrix"><thead><tr><th>发送 ↓ / 接收 →</th>{members.map(id => <th key={id}>{label(id)}</th>)}</tr></thead><tbody>{members.map(a => <tr key={a}><th>{label(a)}</th>{members.map(b => { const l = links.find(x => x.source_id === a && x.target_id === b), state = linkStatus(l); return <td key={b}>{a === b ? "—" : <div className={`network-cell network-${state}`} title={l ? `${l.error || `抖动 ${l.jitter_ms.toFixed(2)} ms`} · ${time(l.observed_at)}` : "尚未测量"}><b>{statuses[state]}</b>{l && (state === "measured" || state === "slow") && <><span>{l.rtt_p95_ms.toFixed(2)} ms</span><span>{l.upload_mbps.toFixed(1)} Mbps</span></>}</div>}</td>; })}</tr>)}</tbody></table></div>
      {!members.length && <p className="empty">先接入至少两台 Agent，再测量网络。</p>}
      <p className="panel-note">每次 5 个 RTT 采样 + 1 MiB 上传，速度包含 HTTP 往返开销，是有界测试的有效速率。它不等于网卡最大带宽，也不能保证模型生成速度。旧版组标签未配置测量策略时仍按原逻辑调度。</p>
      <form onSubmit={measure}><div className="form-two">{([["发送节点", source, setSource], ["接收节点", target, setTarget]] as const).map(([caption, value, setter]) => <label key={caption}>{caption}<select required value={value} onChange={e => setter(e.target.value)}><option value="">选择节点</option>{nodes.filter(n => n.agent?.network_probe).map(n => <option key={n.id} value={n.id}>{n.name}</option>)}</select></label>)}</div><div className="form-actions"><button className="primary" disabled={busy || !source || !target || source === target}>测量发送 → 接收</button><button type="button" onClick={() => { setSource(target); setTarget(source); }}>交换方向</button></div></form>
      <div className="table-wrap"><table><thead><tr><th>最近探测</th><th>状态</th><th>创建时间</th></tr></thead><tbody>{probes.slice(0, 8).map(p => <tr key={p.id}><td>{label(p.source_id)} → {label(p.target_id)}</td><td>{statuses[p.status] || p.status}</td><td>{time(p.created_at)}</td></tr>)}</tbody></table></div>
    </section>
    <section className="panel network-panel"><div className="panel-heading"><div><h2>{form.id && groups.some(g => g.id === form.id) ? "编辑网络组" : "建立网络组"}</h2><p>手动选择 1–8 台机器。修改已有组前需先停止组内的部署。</p></div></div>
      <form onSubmit={save}><div className="form-two"><label>组标识<input required pattern="[A-Za-z0-9._-]+" maxLength={120} value={form.id} onChange={e => setForm({ ...form, id: e.target.value })} /></label><label>显示名称<input required maxLength={120} value={form.name} onChange={e => setForm({ ...form, name: e.target.value })} /></label></div>
        <fieldset className="network-members"><legend>组内节点（{form.node_ids.length}/8）</legend>{nodes.map(n => <label key={n.id}><input type="checkbox" checked={form.node_ids.includes(n.id)} disabled={form.node_ids.length >= 8 && !form.node_ids.includes(n.id)} onChange={e => setForm({ ...form, node_ids: e.target.checked ? [...form.node_ids, n.id] : form.node_ids.filter(id => id !== n.id) })} />{n.name}</label>)}</fieldset>
        <div className="form-two">{([["max_rtt_ms", "最大 RTT（ms）", 0.1, 5000], ["max_jitter_ms", "最大抖动（ms）", 0.1, 5000], ["min_mbps", "最小上传速率（Mbps）", 0.1, 1000000], ["max_age_seconds", "测量有效期（秒）", 30, 3600]] as const).map(([key, title, min, max]) => <label key={key}>{title}<input required type="number" min={min} max={max} step={key === "max_age_seconds" ? 1 : 0.1} value={form[key]} onChange={e => setForm({ ...form, [key]: +e.target.value })} /></label>)}</div>
        <div className="form-actions"><button className="primary" disabled={busy || !form.node_ids.length}>保存网络组</button><button type="button" onClick={() => setForm({ ...blank, node_ids: [] })}>清空表单</button></div>
      </form>
    </section>
  </>;
}
