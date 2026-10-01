export const API_BASE = (import.meta.env.VITE_API_BASE_URL || "/api").replace(/\/$/, "");
export class ApiError extends Error { constructor(message: string, public status: number, public body?: unknown) { super(message); } }
export async function api<T = unknown>(path: string, init: RequestInit = {}): Promise<T> {
  const token = sessionStorage.getItem("platform-token");
  const headers = new Headers(init.headers); headers.set("Content-Type", "application/json");
  if (token) headers.set("Authorization", `Bearer ${token}`);
  let response: Response;
  try { response = await fetch(`${API_BASE}${path}`, { ...init, headers, signal: init.signal ?? AbortSignal.timeout(15000) }); }
  catch { throw new ApiError("无法连接控制面或请求超时", 0); }
  const raw = await response.text(); let body: any;
  try { body = raw ? JSON.parse(raw) : undefined; } catch { throw new ApiError("服务返回了非 JSON 响应", response.status); }
  if (!response.ok) throw new ApiError(body?.error || `${response.status} ${response.statusText}`, response.status, body);
  return body as T;
}
