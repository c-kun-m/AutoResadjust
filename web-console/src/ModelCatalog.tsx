import { FormEvent, useState } from "react";
import { api } from "./api";
import { Artifact } from "./types";

export default function ModelCatalog({ artifacts, reload }: { artifacts: Artifact[]; reload: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [form, setForm] = useState({ id: "", name: "", file: "", revision: "", sha256: "", architecture: "llama", quantization: "", weight: "", layers: "", context_limit: 2048 });
  const patch = (key: string, value: string | number) => setForm(f => ({ ...f, [key]: value }));
  const submit = async (e: FormEvent) => {
    e.preventDefault(); setBusy(true); setError("");
    try {
      const { weight, ...metadata } = form;
      await api("/v1/model-artifacts", { method: "POST", body: JSON.stringify({ ...metadata, format: "gguf", layers: Number(form.layers), weight_mib: Math.ceil(Number(weight) * 1024) }) });
      await reload(); setForm(f => ({ ...f, id: "", name: "", file: "", sha256: "" }));
    } catch (e) { setError(String(e)); } finally { setBusy(false); }
  };
  return <section className="panel"><div className="panel-heading"><div><h2>模型目录 <span className="count-label">{artifacts.length}</span></h2><p>固定文件、版本和校验和；登记后仍需在节点准备权重。</p></div></div>
    {artifacts.map(a => <div className="deployment-card" key={a.id}><b>{a.name}</b><p>{a.id} · {a.quantization} · {(a.weight_mib / 1024).toFixed(2)} GiB · {a.layers} 层</p><small>{a.file} · 版本 {a.revision}</small></div>)}
    <details><summary>登记 GGUF 模型</summary><form onSubmit={submit} className="catalog-form">
      <div className="form-two">{[["id", "唯一标识"], ["name", "显示名称"], ["file", "模型文件名"], ["revision", "权重固定版本"], ["architecture", "模型架构"], ["quantization", "量化方式"]].map(([key, label]) => <label key={key}>{label}<input required value={String(form[key as keyof typeof form])} onChange={e => patch(key, e.target.value)} /></label>)}</div>
      <label>SHA-256 校验和<input required pattern="[a-fA-F0-9]{64}" value={form.sha256} onChange={e => patch("sha256", e.target.value)} /></label>
      <div className="form-two"><label>权重大小（GiB）<input required type="number" min="0.001" step="0.001" value={form.weight} onChange={e => patch("weight", e.target.value)} /></label><label>实际模型层数<input required type="number" min="1" max="1024" value={form.layers} onChange={e => patch("layers", e.target.value)} /></label><label>最大上下文<input required type="number" min="128" max="131072" value={form.context_limit} onChange={e => patch("context_limit", +e.target.value)} /></label></div>
      <p className="panel-note">更换权重或量化方式时使用新的标识，已登记版本不能覆盖。</p>
      {error && <div className="error-banner" role="alert">{error}</div>}<button className="primary" disabled={busy}>{busy ? "登记中…" : "登记模型"}</button>
    </form></details>
  </section>;
}
