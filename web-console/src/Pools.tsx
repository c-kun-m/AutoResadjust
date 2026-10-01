import { FormEvent, useCallback, useEffect, useState } from "react";
import { api } from "./api";
import { Deployment, phases, time } from "./types";

type Pool = { id: string; model_ref: string; backend: string; deployment_ids: string[] };
type RequestStats = { deployment_id: string; active: number; queued: number; requests: number; completed: number; failed: number; canceled: number; rejected: number; queue_timeouts: number; first_token_timeouts: number; idle_timeouts: number; total_timeouts: number; ttft_count: number; ttft_seconds_sum: number; last_outcome?: string; last_completed_at?: string };
type Report = { deployments: RequestStats[]; policy: { concurrency: number; queue_size: number; queue_wait_seconds: number; first_token_seconds: number; idle_seconds: number; total_seconds: number } };
const blank: Pool = { id: "", model_ref: "", backend: "llama_local", deployment_ids: [] };
const outcomes: Record<string, string> = { completed: "完成", canceled: "已取消", queue_canceled: "排队已取消", queue_timeout: "排队超时", first_token_timeout: "首 token 超时", idle_timeout: "生成停滞", total_timeout: "总时限超时", upstream_error: "上游失败", client_write_error: "客户端传输失败" };

export default function Pools({ deployments }: { deployments: Deployment[] }) {
  const [pools, setPools] = useState<Pool[]>([]), [report, setReport] = useState<Report>();
  const [form, setForm] = useState<Pool>(blank), [busy, setBusy] = useState(false), [error, setError] = useState(""), [notice, setNotice] = useState("");
  const refresh = useCallback(async (signal?: AbortSignal) => {
    const [p, r] = await Promise.all([api<Pool[]>("/v1/model-pools", { signal }), api<Report>("/v1/monitor/inference", { signal })]);
    setPools(p); setReport(r); setError("");
  }, []);
  useEffect(() => {
    const controller = new AbortController(); let loading = false;
    const poll = async () => { if (loading) return; loading = true; try { await refresh(controller.signal); } catch (e) { if (!controller.signal.aborted) setError(String(e)); } finally { loading = false; } };
    void poll(); const timer = setInterval(() => void poll(), 5000); return () => { controller.abort(); clearInterval(timer); };
  }, [refresh]);
  const save = async (e: FormEvent) => {
    e.preventDefault(); setBusy(true); setError(""); setNotice("");
    try { await api("/v1/model-pools", { method: "PUT", body: JSON.stringify(form) }); setNotice(`模型入口 ${form.id} 已保存。请求会选择其中可用的部署。`); await refresh(); }
    catch (e) { setError(String(e)); } finally { setBusy(false); }
  };
  const identityOptions = [...new Map(deployments.filter(d => d.spec.model_ref).map(d => { const value = `${d.spec.model_ref}|${d.spec.backend || "llama_rpc"}`; return [value, { value, ref: d.spec.model_ref!, backend: d.spec.backend || "llama_rpc" }]; })).values()];
  const candidates = deployments.filter(d => d.spec.model_ref === form.model_ref && (d.spec.backend || "llama_rpc") === form.backend);
  const stats = report?.deployments ?? [], policy = report?.policy;
  const name = (id: string) => deployments.find(d => d.id === id)?.spec.name || id;
  const existing = pools.some(p => p.id === form.id);
  return <>
    <div className="page-heading"><div><h1>聊天入口与请求</h1><p>将相同版本的部署组成服务池，查看排队、生成与超时。</p></div></div>
    {error && <div className="error-banner" role="alert">{error}</div>}{notice && <p role="status" className="panel-note">{notice}</p>}
    <div className="summary-grid"><article><span>正在推理</span><strong>{stats.reduce((n, s) => n + s.active, 0)}</strong><footer>已获得部署执行名额</footer></article><article><span>正在排队</span><strong>{stats.reduce((n, s) => n + s.queued, 0)}</strong><footer>等待同一部署释放名额</footer></article><article><span>完成 / 失败</span><strong>{stats.reduce((n, s) => n + s.completed, 0)}<small> / {stats.reduce((n, s) => n + s.failed, 0)}</small></strong><footer>从本次控制面启动开始统计</footer></article><article><span>取消 / 拒绝</span><strong>{stats.reduce((n, s) => n + s.canceled, 0)}<small> / {stats.reduce((n, s) => n + s.rejected + s.queue_timeouts, 0)}</small></strong><footer>队列满载或排队超时会拒绝</footer></article></div>
    <section className="panel network-panel"><div className="panel-heading"><div><h2>请求进度</h2><p>按部署统计，模型池与旧部署入口共享额度。</p></div></div>
      {policy && <p className="panel-note">每个部署并发 {policy.concurrency} · 队列 {policy.queue_size} · 排队 {policy.queue_wait_seconds}s · 首 token {policy.first_token_seconds}s · 无进度 {policy.idle_seconds}s · 总时限 {policy.total_seconds}s</p>}
      <div className="table-wrap"><table><thead><tr><th>部署</th><th>执行 / 排队</th><th>完成 / 失败 / 取消</th><th>平均首 token</th><th>超时：排队 / 首 token / 停滞 / 总计时</th><th>最近结果</th></tr></thead><tbody>{stats.map(s => <tr key={s.deployment_id}><td title={s.deployment_id}>{name(s.deployment_id)}</td><td>{s.active} / {s.queued}</td><td>{s.completed} / {s.failed} / {s.canceled}</td><td>{s.ttft_count ? `${(s.ttft_seconds_sum / s.ttft_count).toFixed(2)}s` : "尚无输出观测"}</td><td>{s.queue_timeouts} / {s.first_token_timeouts} / {s.idle_timeouts} / {s.total_timeouts}</td><td>{s.last_outcome ? outcomes[s.last_outcome] || s.last_outcome : "执行中"}<small className="pool-time">{time(s.last_completed_at)}</small></td></tr>)}</tbody></table></div>
      {!stats.length && <p className="empty">发起推理后，这里会显示真实请求状态。</p>}
      <p className="panel-note">首 token 包含排队等待，仅在实际内容或工具调用增量到达时记录；无输出的失败不会记为 0 秒。流式响应开始后的错误单独统计，即使 HTTP 状态已是 200。请求发送后固定在当前部署，不自动重试。</p>
    </section>
    <section className="panel network-panel"><div className="panel-heading"><div><h2>模型服务池</h2><p>客户端使用 GET /v1/models 和 POST /v1/chat/completions，model 填写入口标识。</p></div></div>
      {pools.map(p => <article className="deployment-card" key={p.id}><div className="deployment-title"><h3>{p.id}</h3><button onClick={() => setForm({ ...p, deployment_ids: [...p.deployment_ids] })}>编辑成员</button></div><p>{p.model_ref} · {p.backend}</p><div className="placement-list">{p.deployment_ids.map(id => <div key={id}><b>{name(id)}</b><span>{phases[deployments.find(d => d.id === id)?.phase || "offline"]}</span></div>)}</div></article>)}
      {!pools.length && <p className="empty">先创建引用模型目录的部署，再建立入口。</p>}
      <form onSubmit={save}><div className="form-two"><label>入口标识<input required pattern="[A-Za-z0-9._-]+" maxLength={120} value={form.id} onChange={e => { const match = pools.find(p => p.id === e.target.value); setForm(match ? { ...match, deployment_ids: [...match.deployment_ids] } : { ...form, id: e.target.value }); }} /></label><label>固定模型与后端<select required disabled={existing} value={form.model_ref ? `${form.model_ref}|${form.backend}` : ""} onChange={e => { const option = identityOptions.find(o => o.value === e.target.value); if (option) setForm({ ...form, model_ref: option.ref, backend: option.backend, deployment_ids: [] }); }}><option value="">选择已登记的模型部署</option>{identityOptions.map(o => <option value={o.value} key={o.value}>{o.ref} · {o.backend}</option>)}</select></label></div>
        <fieldset className="network-members"><legend>部署成员（最多 32 个）</legend>{candidates.map(d => <label key={d.id}><input type="checkbox" checked={form.deployment_ids.includes(d.id)} disabled={form.deployment_ids.length >= 32 && !form.deployment_ids.includes(d.id)} onChange={e => setForm({ ...form, deployment_ids: e.target.checked ? [...form.deployment_ids, d.id] : form.deployment_ids.filter(id => id !== d.id) })} />{d.spec.name} · {phases[d.phase] || d.phase}</label>)}</fieldset>
        <p className="panel-note">入口绑定不可变模型版本与执行后端。只有就绪且心跳有效的成员接收新请求；停止或失联的成员会自动退出请求选择。</p>
        <div className="form-actions"><button className="primary" disabled={busy || !form.deployment_ids.length}>保存入口</button><button type="button" onClick={() => setForm({ ...blank, deployment_ids: [] })}>新建入口</button></div>
      </form>
    </section>
  </>;
}
