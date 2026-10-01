import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";

type GPU = { id: string; index?: number; vendor?: string; model?: string; memory_mib: number; free_memory_mib: number; topology_group?: string; utilization_pct?: number; allocated_task_id?: string };
type NodeHealth = "ready" | "degraded" | "draining" | "offline" | string;
type Node = { id: string; name: string; datacenter: string; region?: string; runtime: string; labels?: Record<string, string>; gpus?: GPU[]; health: NodeHealth; last_heartbeat?: string; queue_depth?: number; running_tasks?: number; cost_per_gpu_hour?: number; scheduling_enabled?: boolean; deployment_id?: string };
type Task = { spec: { id: string; name: string; type: string; gpu_count: number }; phase: string; assigned_node_id?: string; failure_reason?: string };
type FormState = { id: string; name: string; datacenter: string; region: string; runtime: string; cost: string; scheduling: boolean; labels: string; gpus: string };
type ApiError = Error & { status?: number };

import { api, API_BASE } from "./api";
const emptyForm = (): FormState => ({ id: "", name: "", datacenter: "", region: "", runtime: "wsl2-docker", cost: "0", scheduling: false, labels: "{}", gpus: "[]" });
const healthLabel: Record<string, string> = { ready: "就绪", degraded: "未连接/降级", draining: "排空中", offline: "离线" };
const phaseLabel: Record<string, string> = { pending: "排队中", assigned: "已分配", running: "执行中", cancelling: "取消中", succeeded: "已完成", failed: "失败", cancelled: "已取消" };
const formatTime = (value?: string) => value && !value.startsWith("0001") ? new Date(value).toLocaleString("zh-CN", { hour12: false }) : "从未上报";

const fresh = (node: Node) => !!node.last_heartbeat && Date.now() - Date.parse(node.last_heartbeat) < 35000;
const available = (node: Node) => node.scheduling_enabled && node.health === "ready" && fresh(node) && !node.deployment_id ? (node.gpus ?? []).filter(gpu => !gpu.allocated_task_id && gpu.free_memory_mib > 0).length : 0;

export default function Nodes() {
  const [nodes, setNodes] = useState<Node[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [form, setForm] = useState<FormState>(emptyForm());
  const [selected, setSelected] = useState<string>("");
  const [editing, setEditing] = useState(false);
  const [error, setError] = useState("");
  const [lastSync, setLastSync] = useState<Date>();
  const [busy, setBusy] = useState("");
  const [taskName, setTaskName] = useState("demo-inference");
  const [taskGPUCount, setTaskGPUCount] = useState(1);

  const refresh = useCallback(async () => {
    const results = await Promise.allSettled([api<Node[]>("/v1/resources"), api<Task[]>("/v1/tasks")]);
    const failures = results.filter((result) => result.status === "rejected");
    if (results[0].status === "fulfilled") setNodes(Array.isArray(results[0].value) ? results[0].value : []);
    if (results[1].status === "fulfilled") setTasks(Array.isArray(results[1].value) ? results[1].value : []);
    setError(failures.length ? "控制面部分数据读取失败，请重试" : "");
    if (!failures.length) setLastSync(new Date());
  }, []);
  useEffect(() => { void refresh(); const timer = window.setInterval(() => void refresh(), 5000); return () => window.clearInterval(timer); }, [refresh]);

  const selectedNode = nodes.find((node) => node.id === selected);
  const nodeTasks = (id: string) => tasks.filter((task) => task.assigned_node_id === id && !["succeeded", "failed", "cancelled"].includes(task.phase));
  const activeNodes = nodes.filter((node) => (node.gpus ?? []).some((gpu) => gpu.allocated_task_id) || nodeTasks(node.id).length > 0);
  const allGPUs = nodes.flatMap((node) => node.gpus ?? []);

  const editNode = (node: Node) => { setEditing(true); setSelected(node.id); setForm({ id: node.id, name: node.name, datacenter: node.datacenter, region: node.region ?? "", runtime: node.runtime, cost: String(node.cost_per_gpu_hour ?? 0), scheduling: node.scheduling_enabled === true, labels: JSON.stringify(node.labels ?? {}, null, 2), gpus: JSON.stringify(node.gpus ?? [], null, 2) }); };
  const resetForm = () => { setEditing(false); setForm(emptyForm()); };
  const submitNode = async (event: FormEvent) => {
    event.preventDefault(); setBusy("node-form"); setError("");
    try {
      const labels = JSON.parse(form.labels || "{}");
      const gpus = JSON.parse(form.gpus || "[]");
      const payload = { id: form.id.trim(), name: form.name.trim(), datacenter: form.datacenter.trim(), region: form.region.trim(), runtime: form.runtime.trim(), cost_per_gpu_hour: Number(form.cost) || 0, scheduling_enabled: form.scheduling, labels, gpus };
      if (!payload.id || !payload.name || !payload.datacenter || !payload.runtime) throw new Error("节点 ID、名称、机房和运行时不能为空");
      await api<Node>(editing ? `/v1/resources/${encodeURIComponent(payload.id)}` : "/v1/resources", { method: editing ? "PUT" : "POST", body: JSON.stringify(payload) });
      resetForm(); await refresh();
    } catch (reason) { setError(reason instanceof Error ? `保存节点失败：${reason.message}` : "保存节点失败"); }
    finally { setBusy(""); }
  };
  const nodeAction = async (id: string, action: "drain" | "enable" | "delete") => {
    setBusy(`${action}:${id}`); setError("");
    try { await api(action === "delete" ? `/v1/resources/${encodeURIComponent(id)}` : `/v1/resources/${encodeURIComponent(id)}/${action}`, { method: action === "delete" ? "DELETE" : "POST" }); if (selected === id) setSelected(""); await refresh(); }
    catch (reason) { setError(reason instanceof Error ? `节点操作失败：${reason.message}` : "节点操作失败"); }
    finally { setBusy(""); }
  };
  const submitTask = async (event: FormEvent) => {
    event.preventDefault(); setBusy("task"); setError("");
    try { await api("/v1/tasks", { method: "POST", body: JSON.stringify({ tenant_id: "console", name: taskName, type: "inference", model_id: "demo-model", gpu_count: taskGPUCount, min_gpu_memory_mib: 1024, priority: 10, idempotency_key: `${taskName}-${Date.now()}` }) }); setTaskName(""); await refresh(); }
    catch (reason) { setError(reason instanceof Error ? `提交任务失败：${reason.message}` : "提交任务失败"); }
    finally { setBusy(""); }
  };
  const cancelTask = async (id: string) => { setBusy(`cancel:${id}`); try { await api(`/v1/tasks/${encodeURIComponent(id)}/cancel`, { method: "POST" }); await refresh(); } catch (reason) { setError(reason instanceof Error ? reason.message : "取消失败"); } finally { setBusy(""); } };

  return <main className="shell">
    <header><div><p className="eyebrow">CONTROL PLANE / NODE ADMINISTRATION</p><h1>异构 AI 算力平台</h1><p className="muted">节点配置、资源状态、GPU 使用和任务调度</p></div><div className="header-actions"><span className="connection connection-connected"><i />每 5 秒同步</span><button onClick={() => void refresh()}>刷新</button></div></header>
    <div className="skeleton-note">节点配置只代表平台登记信息；“已连接/可调度”必须由节点 agent 心跳确认。排空会停止新任务，但允许已有任务完成。</div>
    {error && <div className="alert" role="alert"><strong>操作失败</strong><span>{error}</span><button onClick={() => setError("")}>关闭</button></div>}
    {lastSync && <p className="refresh-meta">最近同步：{formatTime(lastSync.toISOString())} · API：{API_BASE}</p>}
    <section className="metrics"><article><span>登记节点</span><strong>{nodes.length}</strong><small>{nodes.filter((node) => node.health === "ready" && fresh(node)).length} 个已就绪</small></article><article><span>正在使用的节点</span><strong>{activeNodes.length}</strong><small>有任务或 GPU 占用</small></article><article><span>可分配 GPU</span><strong>{nodes.reduce((sum, node) => sum + available(node), 0)}</strong><small>总计 {allGPUs.length} 张</small></article><article><span>任务记录</span><strong>{tasks.length}</strong><small>控制面当前记录</small></article></section>
    <div className="admin-grid">
      <section className="card"><div className="card-title"><h2>{editing ? "编辑节点配置" : "新增算力服务节点"}</h2><span className="tag">节点目录</span></div><form onSubmit={submitNode} className="node-form"><div className="form-two"><label>节点 ID<input value={form.id} disabled={editing} onChange={(event) => setForm({ ...form, id: event.target.value })} placeholder="dc-a-node-01" required /></label><label>节点名称<input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="A 机房 GPU 节点 01" required /></label></div><div className="form-two"><label>数据中心<input value={form.datacenter} onChange={(event) => setForm({ ...form, datacenter: event.target.value })} placeholder="dc-a" required /></label><label>地域<input value={form.region} onChange={(event) => setForm({ ...form, region: event.target.value })} placeholder="cn-east" /></label></div><div className="form-two"><label>运行时<input value={form.runtime} onChange={(event) => setForm({ ...form, runtime: event.target.value })} placeholder="wsl2-docker" required /></label><label>GPU 小时成本<input type="number" min="0" step="0.01" value={form.cost} onChange={(event) => setForm({ ...form, cost: event.target.value })} /></label></div><label>节点标签（JSON）<textarea value={form.labels} onChange={(event) => setForm({ ...form, labels: event.target.value })} rows={3} /></label><label>GPU 配置（JSON，可先留空等待 agent 上报）<textarea value={form.gpus} onChange={(event) => setForm({ ...form, gpus: event.target.value })} rows={4} placeholder={'[{"id":"GPU-uuid","model":"A100","memory_mib":81920,"free_memory_mib":81920,"topology_group":"nvlink-0"}'} /></label><label className="switch"><input type="checkbox" checked={form.scheduling} onChange={(event) => setForm({ ...form, scheduling: event.target.checked })} />允许调度（建议等 agent 心跳后再启用）</label><div className="form-actions"><button className="primary" type="submit" disabled={busy === "node-form"}>{busy === "node-form" ? "保存中…" : editing ? "保存节点" : "登记节点"}</button>{editing && <button type="button" onClick={resetForm}>取消编辑</button>}</div></form></section>
      <section className="card"><div className="card-title"><h2>正在使用的节点</h2><span className="tag">实时占用</span></div>{activeNodes.length === 0 ? <div className="empty-state"><b>当前没有节点承载任务</b><span>提交任务后，这里会显示被选中的节点。</span></div> : <div className="active-list">{activeNodes.map((node) => <button className="active-node" key={node.id} onClick={() => setSelected(node.id)}><span><b>{node.name || node.id}</b><small>{node.datacenter} · {nodeTasks(node.id).length} 个活动任务</small></span><strong>{(node.gpus ?? []).filter((gpu) => gpu.allocated_task_id).length}/{(node.gpus ?? []).length} GPU</strong></button>)}</div>}</section>
    </div>
    <section className="card"><div className="card-title"><h2>算力服务节点</h2><span className="tag">配置 / 状态 / 详情</span></div>{nodes.length === 0 ? <div className="empty-state"><b>还没有登记节点</b><span>使用左侧表单登记第一台算力服务器。</span></div> : <div className="node-table-wrap"><table><thead><tr><th>节点</th><th>机房 / 运行时</th><th>连接状态</th><th>GPU 资源</th><th>任务</th><th>最后心跳</th><th>操作</th></tr></thead><tbody>{nodes.map((node) => { const gpus = node.gpus ?? []; const used = gpus.filter((gpu) => gpu.allocated_task_id).length; const taskCount = nodeTasks(node.id).length; return <tr key={node.id} className={selected === node.id ? "selected-row" : ""}><td><button className="link-button" onClick={() => setSelected(node.id)}><b>{node.name || node.id}</b><small>{node.id}</small></button></td><td>{node.datacenter}<small>{node.runtime}</small></td><td><span className={`status status-${node.health}`}>{fresh(node) ? (healthLabel[node.health] ?? node.health) : "离线 / 未连接"}</span><small>{node.scheduling_enabled ? "允许调度" : "已禁用调度"}</small></td><td>{used}/{gpus.length} 占用<small>{gpus.length ? `${available(node)} 张可分配` : "等待 agent 上报"}</small></td><td>{taskCount} 个活动任务</td><td>{formatTime(node.last_heartbeat)}</td><td><div className="row-actions"><button onClick={() => editNode(node)}>编辑</button>{node.scheduling_enabled ? <button onClick={() => void nodeAction(node.id, "drain")} disabled={busy === `drain:${node.id}`}>排空</button> : <button onClick={() => void nodeAction(node.id, "enable")} disabled={busy === `enable:${node.id}`}>启用</button>}<button className="danger-button" onClick={() => void nodeAction(node.id, "delete")} disabled={busy === `delete:${node.id}`}>删除</button></div></td></tr>; })}</tbody></table></div>}</section>
    {selectedNode && <section className="card detail-card"><div className="card-title"><h2>节点详情：{selectedNode.name || selectedNode.id}</h2><button onClick={() => setSelected("")}>收起</button></div><div className="detail-grid"><div><dl><dt>节点 ID</dt><dd>{selectedNode.id}</dd><dt>数据中心 / 地域</dt><dd>{selectedNode.datacenter} / {selectedNode.region || "—"}</dd><dt>运行时</dt><dd>{selectedNode.runtime}</dd><dt>调度状态</dt><dd>{selectedNode.scheduling_enabled ? "允许调度" : "已禁用"}</dd><dt>队列 / 运行中</dt><dd>{selectedNode.queue_depth ?? 0} / {selectedNode.running_tasks ?? 0}</dd></dl></div><div><h3>GPU 明细</h3>{(selectedNode.gpus ?? []).length === 0 ? <p className="muted">等待 agent 上报 GPU。</p> : <div className="gpu-detail-list">{(selectedNode.gpus ?? []).map((gpu) => <div className="gpu-detail" key={gpu.id}><b>{gpu.model || "未知型号"}</b><span>{gpu.id}</span><small>{Math.round((gpu.free_memory_mib || 0) / 1024)} / {Math.round((gpu.memory_mib || 0) / 1024)} GiB 可用 · {gpu.topology_group || "拓扑未知"}</small><em>{gpu.allocated_task_id ? `占用：${gpu.allocated_task_id}` : "未被平台预留"} · 利用率 {gpu.utilization_pct === undefined ? "未采集" : `${gpu.utilization_pct}%`}</em></div>)}</div>}</div></div></section>}
    <div className="grid"><section className="card"><div className="card-title"><h2>单节点调度验证</h2><span className="tag">仅调度记录</span></div><form onSubmit={submitTask}><label>任务名称<input value={taskName} onChange={(event) => setTaskName(event.target.value)} required /></label><label>GPU 数量<input type="number" min="1" max="8" value={taskGPUCount} onChange={(event) => setTaskGPUCount(Math.max(1, Number(event.target.value) || 1))} /></label><button className="primary" type="submit" disabled={busy === "task"}>{busy === "task" ? "提交中…" : "提交并调度"}</button></form><p className="hint">此入口仅验证旧版单节点调度，不启动模型进程。实际跨节点推理请使用“模型部署”页面。</p></section><section className="card"><div className="card-title"><h2>任务中心</h2><span className="tag">状态可核对</span></div><div className="table-wrap"><table><thead><tr><th>任务</th><th>阶段</th><th>节点</th><th>操作</th></tr></thead><tbody>{tasks.length === 0 ? <tr><td colSpan={4} className="empty">暂无任务</td></tr> : tasks.map((task) => <tr key={task.spec.id}><td><b>{task.spec.name || task.spec.id}</b><small>{task.spec.type}</small></td><td><span className={`phase phase-${task.phase}`}>{phaseLabel[task.phase] ?? task.phase}</span></td><td>{task.assigned_node_id || "等待资源"}</td><td>{["succeeded", "failed", "cancelled"].includes(task.phase) ? "—" : <button onClick={() => void cancelTask(task.spec.id)} disabled={busy === `cancel:${task.spec.id}`}>取消</button>}</td></tr>)}</tbody></table></div></section></div>
  </main>;
}
