/**
 * Portal 会话库 —— 内存态，唯一事实源。
 *
 * 安全规则（TASK-005 约束）：会话 token 是凭据，只保存在 JS 内存里，
 * **绝不写入 localStorage / sessionStorage / cookie**。刷新页面即失效，
 * 重新登录即可（dev-login 秒级完成；生产 OIDC 接入后由会话 cookie 承接，
 * 那是网关侧的 HttpOnly 会话，不属于浏览器脚本可触及的存储）。
 *
 * 用 useSyncExternalStore 而非 Context：路由 beforeLoad 守卫（非组件环境）
 * 也要读会话，模块级单例让两处共享同一事实源。
 */

import { useSyncExternalStore } from "react";
import { bindSessionBridge, devLogin, eapRequest, localLogin, type MeResponse } from "@/lib/api/eap";
import { desktopBridgeToken } from "@/lib/auth/desktopPortal";

export interface SessionState {
	/** 控制面会话 token（内部 JWT，aud=agent-gateway）。仅存在于内存。 */
	token: string;
	user: MeResponse["user"];
	workspaces: MeResponse["workspaces"];
	defaultWorkspace: string;
	/** 当前选中的 Workspace —— 仅影响 Portal 内的展示与配置生成。 */
	selectedWorkspace: string;
}

let current: SessionState | null = null;
const listeners = new Set<() => void>();

function emit() {
	for (const listener of listeners) listener();
}

export function getSession(): SessionState | null {
	return current;
}

export function getSessionToken(): string | undefined {
	return current?.token;
}

export function setSelectedWorkspace(workspaceId: string) {
	if (!current) return;
	current = { ...current, selectedWorkspace: workspaceId };
	emit();
}

export function setSession(next: SessionState) {
	current = next;
	emit();
}

export function clearSession() {
	current = null;
	emit();
}

export function subscribe(listener: () => void): () => void {
	listeners.add(listener);
	return () => listeners.delete(listener);
}

// ---- 会话桥：eap 客户端读 token / 处理 401 ----

bindSessionBridge({
	readToken: getSessionToken,
	onUnauthorized: () => {
		const desktop = getSessionToken() === desktopBridgeToken;
		clearSession();
		if (desktop && typeof window !== "undefined") window.location.assign("/login");
	},
});

// ---- 登录 / 登出 ----

/** dev 模式登录（IdP 替身）：换会话、拉档案。生产接入 OIDC 后由适配器替换。 */
export async function login(email: string, displayName: string, workspaces: string[]): Promise<SessionState> {
	const sessionResp = await devLogin(email, displayName, workspaces);
	// 先挂 token 拉档案；失败则不留半套会话。
	current = {
		token: sessionResp.session_token,
		user: { id: "", display_name: "", email },
		workspaces: [],
		defaultWorkspace: "",
		selectedWorkspace: "",
	};
	emit();
	try {
		const profile = await eapRequest<MeResponse>("/enterprise/v1/profile");
		const next: SessionState = {
			token: sessionResp.session_token,
			user: profile.user,
			workspaces: profile.workspaces,
			defaultWorkspace: profile.default_workspace,
			selectedWorkspace: profile.default_workspace || profile.workspaces[0]?.id || "",
		};
		current = next;
		emit();
		return next;
	} catch (cause) {
		clearSession();
		throw cause;
	}
}

/**
 * OIDC 登录引导：浏览器带着网关 HttpOnly 会话 cookie 回跳后，用该 cookie
 * 换取控制面会话 token（仅在内存）。cookie 每次引导都由网关重新校验；
 * token 本身绝不写入任何脚本可触及的持久存储。
 */
export async function bootstrapOIDCSession(): Promise<SessionState | null> {
	try {
		const profile = await eapRequest<{
			user: MeResponse["user"];
			workspaces: MeResponse["workspaces"];
			default_workspace: string;
			session_token: string;
		}>("/enterprise/v1/auth/oidc/profile");
		const next: SessionState = {
			token: profile.session_token,
			user: profile.user,
			workspaces: profile.workspaces,
			defaultWorkspace: profile.default_workspace,
			selectedWorkspace: profile.default_workspace || profile.workspaces[0]?.id || "",
		};
		current = next;
		emit();
		return next;
	} catch {
		return null;
	}
}

/** Desktop guest bootstrap: profile comes from the host; the API key never enters page JavaScript. */
export async function bootstrapDesktopSession(): Promise<SessionState | null> {
	if (typeof window === "undefined" || !window.yuanpuPortal) return null;
	try {
		const profile = await window.yuanpuPortal.restore();
		if (!profile) return null;
		const next: SessionState = {
			token: desktopBridgeToken,
			user: profile.user,
			workspaces: profile.workspaces,
			defaultWorkspace: profile.default_workspace,
			selectedWorkspace: profile.default_workspace || profile.workspaces[0]?.id || "",
		};
		setSession(next);
		return next;
	} catch {
		return null;
	}
}

/**
 * 内置账密登录（TASK-027）：username/password 换会话 token（仅内存），
 * 随后拉档案绑定工作区。与 dev-login 共享同一会话建立逻辑。
 */
export class PasswordChangeRequired extends Error {
	constructor() {
		super("password change required");
		this.name = "PasswordChangeRequired";
	}
}

export async function loginWithPassword(username: string, password: string): Promise<SessionState> {
	const sessionResp = await localLogin(username, password);
	if (sessionResp.password_change_required || !sessionResp.session_token) throw new PasswordChangeRequired();
	current = {
		token: sessionResp.session_token,
		user: { id: "", display_name: "", email: "" },
		workspaces: [],
		defaultWorkspace: "",
		selectedWorkspace: "",
	};
	emit();
	try {
		const profile = await eapRequest<MeResponse>("/enterprise/v1/profile");
		const next: SessionState = {
			token: sessionResp.session_token,
			user: profile.user,
			workspaces: profile.workspaces,
			defaultWorkspace: profile.default_workspace,
			selectedWorkspace: profile.default_workspace || profile.workspaces[0]?.id || "",
		};
		current = next;
		emit();
		return next;
	} catch (cause) {
		clearSession();
		throw cause;
	}
}

/** React 绑定 —— 组件里读同一份内存会话。 */
export function useSession(): SessionState | null {
	return useSyncExternalStore(subscribe, getSession, getSession);
}

export function logout() {
	clearSession();
}
