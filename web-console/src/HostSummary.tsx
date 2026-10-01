import { Node, time } from "./types";
export default function HostSummary({ nodes }: { nodes: Node[] }) {
  return <section className="panel"><div className="panel-heading"><div><h2>主机内存与 CPU</h2><p>本机内存分担显存前，先核对实际可用量和已预留量。</p></div></div><div className="service-table"><table><thead><tr><th>节点</th><th>总内存</th><th>当前可用</th><th>部署预留</th><th>CPU 使用率</th><th>最近心跳</th></tr></thead><tbody>{nodes.map(n => {
    const fresh = !!n.last_heartbeat && Date.now() - Date.parse(n.last_heartbeat) < 35000;
    const gib = (v: number) => `${(v / 1024).toFixed(1)} GiB`;
    return <tr key={n.id}><td>{n.name || n.id}</td><td>{n.host ? gib(n.host.memory_total_mib) : "未采集"}</td><td>{fresh && n.host ? gib(n.host.memory_available_mib) : "等待有效心跳"}</td><td>{gib(n.reserved_ram_mib || 0)}</td><td>{fresh && n.host?.cpu_utilization_pct !== undefined ? `${n.host.cpu_utilization_pct.toFixed(1)}%` : "未采集"}</td><td>{time(n.last_heartbeat)}</td></tr>;
  })}</tbody></table>{!nodes.length && <div className="empty-panel">等待节点接入</div>}</div></section>;
}
