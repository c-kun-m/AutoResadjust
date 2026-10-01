import { FormEvent, useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";
import { Badge } from "./Monitor";
import ModelCatalog from "./ModelCatalog";
import { Artifact, Deployment, Node, Plan } from "./types";

const backendNames: Record<string, string> = { llama_rpc: "局域网多机协作", llama_local: "单机 GPU + 内存", petals: "私有 Petals 模型块协作" };
export default function Models({ deployments, nodes, refresh, setError, monitor }: { deployments: Deployment[]; nodes: Node[]; refresh: () => Promise<void>; setError: (s: string) => void; monitor: (id: string) => void }) {
  const [busy, setBusy] = useState(false);
  const [preview, setPreview] = useState<Plan>();
  const [previewError, setPreviewError] = useState("");
  const [artifacts, setArtifacts] = useState<Artifact[]>([]);
  const key = useRef("");
  const [form, setForm] = useState({ name: "协同模型服务", backend: "llama_rpc", model_ref: "", model_file: "", weight: "16", reserve: "1", kv: "1", context_size: 2048, min_nodes: 2, max_nodes: 3, network_group: "lan-default", coordinator_id: "", layers: "", gpu_layers: 10, ram: "24" });
  const [prompt, setPrompt] = useState("你好，请介绍一下你自己。");
  const [answer, setAnswer] = useState("");
	const chatAbort = useRef<AbortController | null>(null);
	const [chatID, setChatID] = useState("");
	useEffect(() => () => chatAbort.current?.abort(), []);
  const local = form.backend === "llama_local";
  const petals = form.backend === "petals";
  const loadArtifacts = useCallback(async () => { setArtifacts(await api<Artifact[]>("/v1/model-artifacts")); }, []);
  useEffect(() => { void loadArtifacts().catch(e => setError(String(e))); }, [loadArtifacts, setError]);
  const patch = (v: Partial<typeof form>) => { setForm(f => ({ ...f, ...v })); setPreview(undefined); setPreviewError(""); key.current = ""; };
  const spec = () => ({ name: form.name, backend: form.backend, model_ref: form.model_ref, model_file: form.model_file, weight_mib: Math.ceil(Number(form.weight) * 1024), reserve_mib: Math.ceil(Number(form.reserve) * 1024), kv_cache_mib: Math.ceil(Number(form.kv) * 1024), context_size: form.context_size, min_nodes: local ? 1 : form.min_nodes, max_nodes: local ? 1 : form.max_nodes, network_group: form.network_group, coordinator_id: form.coordinator_id, ...(local ? { layers: Number(form.layers), gpu_layers: form.gpu_layers, ram_mib: Math.ceil(Number(form.ram) * 1024) } : {}) });
  const selectArtifact = (id: string) => {
    const a = artifacts.find(a => a.id === id);
    patch(a ? { model_ref: id, model_file: a.file, weight: String(a.weight_mib / 1024), layers: String(a.layers), context_size: Math.min(2048, a.context_limit) } : { model_ref: "" });
  };
  const plan = async () => {
    setBusy(true); setPreviewError("");
    try { const r = await api<{ plan: Plan }>("/v1/deployments/preview", { method: "POST", body: JSON.stringify(spec()) }); setPreview(r.plan); }
    catch (e) { if (e instanceof ApiError) { setPreview((e.body as { plan?: Plan })?.plan); setPreviewError(e.message); } else { setPreviewError(String(e)); } }
    finally { setBusy(false); }
  };
  const deploy = async (e: FormEvent) => {
    e.preventDefault(); setBusy(true); setPreviewError("");
    if (!key.current) key.current = typeof crypto.randomUUID === "function" ? crypto.randomUUID() : `deploy-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    try { await api("/v1/deployments", { method: "POST", body: JSON.stringify({ ...spec(), idempotency_key: key.current }) }); key.current = ""; await refresh(); }
    catch (e) { setPreviewError(e instanceof Error ? e.message : "部署失败"); } finally { setBusy(false); }
  };
  const stop = async (id: string) => { setBusy(true); try { await api(`/v1/deployments/${encodeURIComponent(id)}/stop`, { method: "POST" }); await refresh(); } catch (e) { setError(String(e)); } finally { setBusy(false); } };
  const test = async (id: string) => {
    setBusy(true); setAnswer("");
    const controller = new AbortController(); chatAbort.current = controller; setChatID(id);
    const timeout = setTimeout(() => controller.abort(), 125000);
    try { const r = await api<{ choices?: { message?: { content?: string } }[] }>(`/v1/deployments/${id}/inference/v1/chat/completions`, { method: "POST", body: JSON.stringify({ messages: [{ role: "user", content: prompt }], max_tokens: 128, stream: false }), signal: controller.signal }); setAnswer(r.choices?.[0]?.message?.content || JSON.stringify(r)); }
    catch (e) { setAnswer(controller.signal.aborted ? "请求已取消或客户端等待超时。" : String(e)); } finally { clearTimeout(timeout); chatAbort.current = null; setChatID(""); setBusy(false); }
  };
  return <><div className="page-heading"><div><p>COLLABORATIVE INFERENCE</p><h1>选择适合这些机器的部署方式</h1><span>局域网内共同承载权重，或用本机内存分担显存压力。</span></div></div>
    <div className="model-layout"><section className="panel"><div className="panel-heading"><h2>创建模型部署</h2></div>
      <form onSubmit={deploy}>
        <label>执行方式<select value={form.backend} onChange={e => patch({ backend: e.target.value, model_ref: "", model_file: "" })}>{Object.entries(backendNames).map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select></label>
        <label>服务名称<input value={form.name} onChange={e => patch({ name: e.target.value })} required /></label>
        <label>模型版本<select required={petals} value={form.model_ref} onChange={e => selectArtifact(e.target.value)}><option value="">{petals ? "选择已封装的 safetensors 模型" : "直接填写已有 GGUF 文件"}</option>{artifacts.filter(a => (a.format === "safetensors") === petals).map(a => <option key={a.id} value={a.id}>{a.name} · {a.quantization}</option>)}</select></label>
        <label>{petals ? "封装模型目录名" : "GGUF 文件名"}<input value={form.model_file} readOnly={!!form.model_ref || petals} onChange={e => patch({ model_file: e.target.value })} placeholder={petals ? "从模型目录选择" : "model-Q8_0.gguf"} required /><small>{petals ? "所有候选节点准备同一只读模型目录，启动时逐文件校验。" : "放在协调节点的模型目录中；目录模型启动时验证校验和。"}</small></label>
        <div className="form-two">
          <label>权重大小（GiB）<input type="number" min="0.001" step="any" readOnly={!!form.model_ref} value={form.weight} onChange={e => patch({ weight: e.target.value })} required /></label>
          <label>上下文长度<input type="number" min="128" max="131072" value={form.context_size} onChange={e => patch({ context_size: +e.target.value })} /></label>
          <label>每卡运行预留（GiB）<input type="number" min="0.1" step="0.1" value={form.reserve} onChange={e => patch({ reserve: e.target.value })} /></label>
          <label>每卡 KV 缓存（GiB）<input type="number" min="0.1" step="0.1" value={form.kv} onChange={e => patch({ kv: e.target.value })} /></label>
          {local ? <><label>实际模型层数<input type="number" min="1" max="1024" readOnly={!!form.model_ref} value={form.layers} onChange={e => patch({ layers: e.target.value })} required /></label><label>放到 GPU 的层数<input type="number" min="0" max={Number(form.layers) || 1024} value={form.gpu_layers} onChange={e => patch({ gpu_layers: +e.target.value })} required /></label><label>主机内存预留（GiB）<input type="number" min="1" step="0.1" value={form.ram} onChange={e => patch({ ram: e.target.value })} required /></label></> : <><label>最少节点<input type="number" min="2" max="8" value={form.min_nodes} onChange={e => patch({ min_nodes: +e.target.value })} /></label><label>最多节点<input type="number" min="2" max="8" value={form.max_nodes} onChange={e => patch({ max_nodes: +e.target.value })} /></label></>}
        </div>
        {local && <p className="panel-note">内存至少预留权重、KV 和运行空间之和，另给系统保留 4 GiB。实际容量须通过加载和预热验证。</p>}
        {petals && <p className="panel-note">需要已测量的私有网络组和同一私有引导配置。按连续模型块分配，加载 RAM 自动按封装清单预留，另保留系统内存 4 GiB；当前支持 Llama 文本聊天。</p>}
        <label>可信网络组<input value={form.network_group} onChange={e => patch({ network_group: e.target.value })} required /></label>
        <label>{local ? "运行节点" : "协调节点"}<select value={form.coordinator_id} onChange={e => patch({ coordinator_id: e.target.value })}><option value="">自动选择（候选节点须准备权重文件）</option>{nodes.map(n => <option key={n.id} value={n.id}>{n.name || n.id}{n.host ? ` · 可用内存 ${(n.host.memory_available_mib / 1024).toFixed(1)} GiB` : " · 内存未采集"}</option>)}</select></label>
        <div className="form-actions"><button type="button" disabled={busy || !form.model_file} onClick={() => void plan()}>预览分配</button><button className="primary" disabled={busy}>{busy ? "处理中…" : "创建部署"}</button></div>
      </form>
      {previewError && <div className="error-banner" role="alert">{previewError}</div>}
      {preview && <div className="plan-result"><h3>资源分配预览</h3>{preview.placements?.map(p => <div key={p.node_id}><b>{p.node_id}{p.coordinator ? " · 协调节点" : ""}</b><span>{p.petals ? `块 ${p.start_block ?? 0}:${p.end_block} · ` : ""}GPU 权重约 {(p.weight_share_mib / 1024).toFixed(2)} GiB{p.ram_mib ? ` · RAM ${(p.ram_mib / 1024).toFixed(1)} GiB` : ""}</span></div>)}{Object.entries(preview.rejections ?? {}).map(([id, reasons]) => <p key={id}><b>{id}</b>：{reasons.join("；")}</p>)}<p>{preview.warning}</p></div>}
    </section><div><section className="panel"><div className="panel-heading"><h2>模型部署 <span className="count-label">{deployments.length}</span></h2></div>
      {!deployments.length && <div className="empty-panel"><b>还没有部署</b><p>先接入并启用节点，再选择执行方式。</p></div>}
      {deployments.map(d => <article key={d.id} className="deployment-card"><div className="deployment-title"><h3>{d.spec.name}</h3><Badge status={d.phase} /></div><p>{backendNames[d.spec.backend || "llama_rpc"]} · {d.spec.model_file}</p><div className="deployment-message">{d.message}</div><div className="placement-list">{d.plan.placements?.map(p => { const worker = d.workers?.[p.node_id]; const state = worker && !["stopped", "failed"].includes(d.phase) && Date.now() - Date.parse(worker.observed_at) > 35000 ? "offline" : (d.spec.backend === "llama_local" ? worker?.model_state : worker?.rpc_state) || "starting"; return <div key={p.node_id}><span><b>{p.node_id}</b><small>{p.petals ? `块 ${p.start_block ?? 0}:${p.end_block} · ` : ""}GPU 权重约 {(p.weight_share_mib / 1024).toFixed(2)} GiB{p.ram_mib ? ` · RAM ${(p.ram_mib / 1024).toFixed(1)} GiB` : ""}</small></span><Badge status={state} /></div>; })}</div>
        {d.phase === "ready" && <div className="endpoint"><span>推理入口</span><code>/api/v1/deployments/{d.id}/inference/v1/chat/completions</code></div>}<div className="form-actions"><button onClick={() => monitor(d.id)}>查看服务监控</button>{!["stopped", "failed"].includes(d.phase) && <button className="danger-button" disabled={busy || d.phase === "stopping"} onClick={() => void stop(d.id)}>停止部署</button>}</div>
        {d.phase === "ready" && <details><summary>测试推理</summary><textarea aria-label="测试提问" value={prompt} onChange={e => setPrompt(e.target.value)} /><button disabled={busy} onClick={() => void test(d.id)}>发送</button>{chatID === d.id && <button onClick={() => chatAbort.current?.abort()}>取消请求</button>}{answer && <pre className="answer-view">{answer}</pre>}</details>}
      </article>)}
    </section><ModelCatalog artifacts={artifacts} reload={loadArtifacts} /></div></div>
  </>;
}
