/**
 * EAP 企业 API 类型化客户端 —— 与 contracts/schemas/v1/enterprise/ 的黄金
 * fixture 一一对应。
 *
 * 错误处理：网关以冻结的 Problem schema 返回失败
 * （{ code, message, trace_id?, retriable }）。这里解析成 EapProblem 抛出，
 * UI 层负责展示 code 与 trace_id（排障时把 trace_id 交给管理员去审计库查）。
 *
 * 凭据：控制面会话（dev-login 签发的内部 JWT）保存在内存会话库
 * （lib/auth/session.ts），**绝不落 localStorage/sessionStorage**。
 */

import { desktopBridgeToken } from "@/lib/auth/desktopPortal";

export interface EapProblemShape {
	code: string;
	message: string;
	trace_id?: string;
	retriable?: boolean;
}

/** 归一化的企业 API 错误 —— 携带冻结契约的全部字段。 */
export class EapProblem extends Error {
	readonly code: string;
	readonly traceId?: string;
	readonly retriable: boolean;
	readonly status: number;

	constructor(shape: EapProblemShape, status: number) {
		super(shape.message);
		this.name = "EapProblem";
		this.code = shape.code;
		this.traceId = shape.trace_id;
		this.retriable = shape.retriable ?? false;
		this.status = status;
	}
}

function authHeaders(): Record<string, string> {
	const token = getSessionToken();
	if (!token) return {};
	return { Authorization: `Bearer ${token}` };
}

/** 会话 token 的读取回调，由 session 模块注入（避免循环依赖）。 */
let sessionTokenReader: () => string | undefined = () => undefined;
/** 会话失效回调 —— 401 时清会话并回登录页。 */
let unauthorizedHandler: () => void = () => undefined;

export function bindSessionBridge(bridge: { readToken: () => string | undefined; onUnauthorized: () => void }) {
	sessionTokenReader = bridge.readToken;
	unauthorizedHandler = bridge.onUnauthorized;
}

export interface RequestOptions {
	method?: "GET" | "POST" | "DELETE" | "PATCH" | "PUT";
	body?: unknown;
	/** 登录、首次改密等公开端点不携带现有会话，也不触发会话过期处理。 */
	auth?: "session" | "none";
	/** 显式覆盖 Authorization（如用一次性 agk key 调试），一般不需要。 */
	bearer?: string;
	headers?: Record<string, string>;
}

export async function eapRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
	const { method = "GET", body, auth = "session", bearer, headers: extraHeaders } = options;
	const headers: Record<string, string> = {
		...extraHeaders,
		...(bearer ? { Authorization: `Bearer ${bearer}` } : auth === "session" ? authHeaders() : {}),
	};
	if (body !== undefined) headers["Content-Type"] = "application/json";

	let response: Response;
	try {
		if (auth === "session" && !bearer && sessionTokenReader() === desktopBridgeToken && window.yuanpuPortal) {
			const result = await window.yuanpuPortal.request({
				path,
				method,
				body: body !== undefined ? JSON.stringify(body) : undefined,
				workspace: extraHeaders?.["X-Agent-Workspace"],
			});
			response = new Response(result.status === 204 ? null : typeof result.body === "string" ? result.body : JSON.stringify(result.body), {
				status: result.status,
				headers: { "content-type": result.contentType },
			});
		} else {
			response = await fetch(path, {
				method,
				headers,
				body: body !== undefined ? JSON.stringify(body) : undefined,
			});
		}
	} catch {
		throw new EapProblem({ code: "network_error", message: "网络错误：无法连接网关", retriable: true }, 0);
	}

	if (response.status === 401 && auth === "session" && !bearer) {
		unauthorizedHandler();
		throw new EapProblem({ code: "unauthenticated", message: "会话已失效，请重新登录" }, 401);
	}

	if (!response.ok) {
		let problem: EapProblemShape = {
			code: `http_${response.status}`,
			message: `请求失败（${response.status}）`,
		};
		try {
			const data = (await response.json()) as Record<string, unknown>;
			if (data && typeof data === "object" && typeof data.code === "string") {
				problem = {
					code: data.code,
					message: typeof data.message === "string" ? data.message : problem.message,
					trace_id: typeof data.trace_id === "string" ? data.trace_id : undefined,
					retriable: data.retriable === true,
				};
			}
		} catch {
			// 响应体不是 JSON —— 保留默认 problem
		}
		throw new EapProblem(problem, response.status);
	}

	if (response.status === 204) return undefined as T;
	return (await response.json()) as T;
}

// ---- 契约类型（与 packages/protocol-enterprise / fixtures 对齐） ----

export interface EapUser {
	id: string;
	display_name: string;
	email: string;
	organization_id?: string;
	department_id?: string;
}

export interface EapWorkspace {
	id: string;
	name: string;
	default?: boolean;
	roles?: string[];
}

export interface MeResponse {
	user: EapUser;
	workspaces: EapWorkspace[];
	default_workspace: string;
}

export interface SessionResponse {
	session_token: string;
	token_type: string;
	expires_in: number;
	/** 一次性密码（管理员新建或初始管理员）：先改密，此时不签发会话。 */
	password_change_required?: boolean;
}

export interface APIKeyCreateRequest {
	name: string;
	environment?: "live" | "test";
	expires_at?: string | null;
}

export interface APIKeyCreated {
	key_id: string;
	key: string;
	prefix: string;
	name: string;
	created_at: string;
	expires_at: string | null;
}

export interface APIKeyView {
	key_id: string;
	prefix: string;
	name: string;
	created_at: string;
	expires_at: string | null;
	last_used_at: string | null;
	last_ip: string | null;
	client_id: string | null;
	revoked_at: string | null;
}

export interface TokenExchangeResponse {
	access_token: string;
	token_type: string;
	expires_in: number;
}

// ---- 端点 ----

export function devLogin(email: string, displayName: string, workspaces: string[]) {
	return eapRequest<SessionResponse>("/enterprise/v1/auth/dev-login", {
		method: "POST",
		auth: "none",
		body: { email, display_name: displayName, workspaces },
	});
}

export function fetchProfile() {
	return eapRequest<MeResponse>("/enterprise/v1/profile");
}

export function createKey(req: APIKeyCreateRequest) {
	return eapRequest<APIKeyCreated>("/enterprise/v1/keys", { method: "POST", body: req });
}

export function listKeys() {
	return eapRequest<APIKeyView[]>("/enterprise/v1/keys");
}

export function revokeKey(keyId: string) {
	return eapRequest<void>(`/enterprise/v1/keys/${encodeURIComponent(keyId)}`, { method: "DELETE" });
}

/** 内置账密登录（TASK-027）：需网关开启 EAP_LOCAL_AUTH。 */
export function localLogin(username: string, password: string) {
	return eapRequest<SessionResponse>("/enterprise/v1/auth/local/login", {
		method: "POST",
		auth: "none",
		body: { username, password },
	});
}

export function exchangeToken(audience: string, workspace: string) {
	return eapRequest<TokenExchangeResponse>("/enterprise/v1/token/exchange", {
		method: "POST",
		bearer: getSessionToken(),
		body: { audience },
		headers: { "X-Agent-Workspace": workspace },
	});
}

function getSessionToken(): string | undefined {
	return sessionTokenReader();
}
