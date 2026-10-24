/**
 * MindLink 中心业务 API —— 知识库、审核与整理、成员与团队、能力目录、
 * 资讯与设备登录确认。
 *
 * 为什么另起一个模块：lib/api/eap.ts 是 EAP 底座的契约；这里对应
 * server/eap/api/mindlink-openapi.yaml，类型直接取自它生成的
 * schema.gen.ts（@contract/schema），与桌面端 SDK 同源，不手抄字段。
 *
 * 约束：
 * - 所有请求带当前选中的 Workspace（X-Agent-Workspace）；会话 token 由
 *   eapRequest 从内存会话读取，本模块不碰凭据存储。
 * - 附件下载需要鉴权头，只能 fetch 成 Blob 再交给浏览器保存，不拼直链。
 * - Wiki HTML 由服务端渲染并按白名单清洗，页面直接展示，不在前端再渲染 Markdown。
 */

import type { components } from "@contract/schema";
import { EapProblem, eapRequest } from "@/lib/api/eap";
import { getSession, getSessionToken } from "@/lib/auth/session";
import { desktopBridgeToken } from "@/lib/auth/desktopPortal";

type Schemas = components["schemas"];
export type Resource = Schemas["Resource"];
export type WikiTree = Schemas["WikiTree"];
export type WikiNode = Schemas["WikiNode"];
export type WikiNodeInput = Schemas["WikiNodeInput"];
export type WikiPage = Schemas["WikiPage"];
export type PageAttachment = Schemas["PageAttachment"];
export type WikiSearchResult = Schemas["WikiSearchResult"];
export type CurationProposal = Schemas["CurationProposal"];
export type CatalogItem = Schemas["CatalogItem"];
export type Member = Schemas["Member"];
export type MemberCreateRequest = Schemas["MemberCreateRequest"];
export type MemberCreated = Schemas["MemberCreated"];
export type MemberUpdateRequest = Schemas["MemberUpdateRequest"];
export type Group = Schemas["Group"];
export type Permission = Schemas["Permission"];
export type Role = Schemas["Role"];
export type RoleCatalog = Schemas["RoleCatalog"];
export type RoleInput = Schemas["RoleInput"];
export type MyPermissions = Schemas["MyPermissions"];
export type SkillPublish = Schemas["SkillPublish"];
export type SkillVersion = Schemas["SkillVersion"];
export type SharedSkill = Schemas["SharedSkill"];
export type SkillContent = Schemas["SkillContent"];

function workspaceHeaders(): Record<string, string> {
	const workspace = getSession()?.selectedWorkspace;
	return workspace ? { "X-Agent-Workspace": workspace } : {};
}

function get<T>(path: string) {
	return eapRequest<T>(path, { headers: workspaceHeaders() });
}

function send<T>(method: "POST" | "PATCH" | "PUT" | "DELETE", path: string, body?: unknown) {
	return eapRequest<T>(path, { method, body, headers: workspaceHeaders() });
}

const enc = encodeURIComponent;

function query(params: Record<string, string | undefined>): string {
	const qs = new URLSearchParams();
	for (const [key, value] of Object.entries(params)) if (value) qs.set(key, value);
	const text = qs.toString();
	return text ? `?${text}` : "";
}

// ---- 资讯与组织资源 ----

export async function listResources(params: { q?: string; kind?: Resource["kind"] } = {}): Promise<Resource[]> {
	return (await get<{ resources: Resource[] }>(`/enterprise/v1/resources${query(params)}`)).resources;
}

export const getResource = (id: string) => get<Resource>(`/enterprise/v1/resources/${enc(id)}`);

// ---- 知识库 ----

export const getWikiTree = () => get<WikiTree>("/enterprise/v1/wiki/tree");

export async function searchWiki(q: string): Promise<WikiSearchResult[]> {
	return (await get<{ results: WikiSearchResult[] }>(`/enterprise/v1/wiki/search${query({ q })}`)).results;
}

export const getWikiPage = (id: string) => get<WikiPage>(`/enterprise/v1/wiki/pages/${enc(id)}`);

/** 路径逐段编码，斜杠保留给网关的 {path...} 通配。 */
export function getWikiPageByPath(path: string) {
	const encoded = path.split("/").filter(Boolean).map(enc).join("/");
	return get<WikiPage>(`/enterprise/v1/wiki/by-path/${encoded}`);
}

export const createWikiNode = (input: WikiNodeInput) => send<WikiNode>("POST", "/enterprise/v1/wiki/nodes", input);

export async function downloadWikiAttachment(pageId: string, file: PageAttachment): Promise<void> {
	const token = getSessionToken();
	const path = `/enterprise/v1/wiki/pages/${enc(pageId)}/attachments/${enc(file.sha256)}`;
	let blob: Blob;
	if (token === desktopBridgeToken && window.yuanpuPortal) {
		const result = await window.yuanpuPortal.request({ path, workspace: getSession()?.selectedWorkspace, binary: true });
		if (result.status < 200 || result.status >= 300 || typeof result.body !== "string") {
			throw new EapProblem({ code: `http_${result.status}`, message: "附件下载失败" }, result.status);
		}
		blob = new Blob([Uint8Array.from(atob(result.body), (char) => char.charCodeAt(0))], { type: result.contentType });
	} else {
		const response = await fetch(path, {
			headers: { ...workspaceHeaders(), ...(token ? { Authorization: `Bearer ${token}` } : {}) },
		});
		if (!response.ok) throw new EapProblem({ code: `http_${response.status}`, message: "附件下载失败" }, response.status);
		blob = await response.blob();
	}
	const url = URL.createObjectURL(blob);
	const anchor = document.createElement("a");
	anchor.href = url;
	anchor.download = file.name;
	anchor.click();
	setTimeout(() => URL.revokeObjectURL(url), 30_000);
}

// ---- 审核与整理 ----

export async function reviewQueue(): Promise<Resource[]> {
	return (await get<{ resources: Resource[] }>("/enterprise/v1/review/queue")).resources;
}

export const approveResource = (id: string) => send<Resource>("POST", `/enterprise/v1/resources/${enc(id)}/approve`);
export const rejectResource = (id: string) => send<Resource>("POST", `/enterprise/v1/resources/${enc(id)}/reject`);

export async function listCurationProposals(): Promise<CurationProposal[]> {
	return (await get<{ proposals: CurationProposal[] }>("/enterprise/v1/curation/proposals")).proposals;
}

export const decideCurationProposal = (id: string, decision: "accept" | "reject" | "revert") =>
	send<CurationProposal>("POST", `/enterprise/v1/curation/proposals/${enc(id)}/decision`, { decision });

export const requestCurationSweep = () => send<{ queued: boolean }>("POST", "/enterprise/v1/curation/sweeps");

// ---- 能力目录 ----

export async function searchCatalog(q: string): Promise<CatalogItem[]> {
	return (await get<Schemas["CatalogSearch"]>(`/enterprise/v1/catalog/search${query({ q })}`)).items;
}

// ---- 成员与团队（管理员） ----

export async function listMembers(): Promise<Member[]> {
	return (await get<{ members: Member[] }>("/enterprise/v1/admin/members")).members;
}

export const createMember = (input: MemberCreateRequest) => send<MemberCreated>("POST", "/enterprise/v1/admin/members", input);
export const updateMember = (userId: string, input: MemberUpdateRequest) =>
	send<Member>("PATCH", `/enterprise/v1/admin/members/${enc(userId)}`, input);
export const removeMember = (userId: string) => send<void>("DELETE", `/enterprise/v1/admin/members/${enc(userId)}`);

export async function listGroups(): Promise<Group[]> {
	return (await get<{ groups: Group[] }>("/enterprise/v1/admin/groups")).groups;
}

export const createGroup = (name: string) => send<Group>("POST", "/enterprise/v1/admin/groups", { name });
export const renameGroup = (id: string, name: string) => send<Group>("PATCH", `/enterprise/v1/admin/groups/${enc(id)}`, { name });
export const deleteGroup = (id: string) => send<void>("DELETE", `/enterprise/v1/admin/groups/${enc(id)}`);

// ---- 角色与权限 ----

export const listRoles = () => get<RoleCatalog>("/enterprise/v1/admin/roles");
export const putRole = (key: string, input: RoleInput) => send<Role>("PUT", `/enterprise/v1/admin/roles/${enc(key)}`, input);
/** 删除自定义角色（同时撤销其授予），或把内置角色恢复默认。 */
export const deleteRole = (key: string) => send<void>("DELETE", `/enterprise/v1/admin/roles/${enc(key)}`);
export const myPermissions = () => get<MyPermissions>("/enterprise/v1/me/permissions");

// ---- 技能共享 ----

export async function listMySkills(): Promise<SharedSkill[]> {
	return (await get<{ skills: SharedSkill[] }>("/enterprise/v1/me/skills")).skills;
}
export async function listSkillReviews(): Promise<SharedSkill[]> {
	return (await get<{ skills: SharedSkill[] }>("/enterprise/v1/skill-reviews")).skills;
}
export const publishSkill = (name: string, input: SkillPublish) =>
	send<SkillVersion>("POST", `/enterprise/v1/skills/${enc(name)}/versions`, input);
export const setSkillEnabled = (name: string, on: boolean) => send<void>(on ? "PUT" : "DELETE", `/enterprise/v1/me/skills/${enc(name)}`);
/** groupId 为空表示全所必装。 */
export const setSkillRequired = (name: string, groupId: string, on: boolean) =>
	send<void>(on ? "PUT" : "DELETE", `/enterprise/v1/skills/${enc(name)}/requirement${query({ group_id: groupId })}`);
export const getSkillContent = (name: string, version: string) =>
	get<SkillContent>(`/enterprise/v1/skills/${enc(name)}/versions/${enc(version)}/content`);
/** 通过 = 审核 + 发布（签名）；已审核未发布的版本只需发布。 */
export async function approveSkill(skill: SharedSkill): Promise<void> {
	const base = `/enterprise/v1/skills/${enc(skill.name)}/versions/${enc(skill.version)}`;
	if (skill.status === "pending_review") await send<unknown>("POST", `${base}/review`, {});
	await send<unknown>("POST", `${base}/release`, {});
}
export const rejectSkill = (name: string, version: string, reason: string) =>
	send<unknown>("POST", `/enterprise/v1/skills/${enc(name)}/versions/${enc(version)}/reject`, { reason });

// ---- 设备登录确认与首次改密 ----

export const approveDevice = (userCode: string) => send<unknown>("POST", "/enterprise/v1/auth/device/approve", { user_code: userCode });
export const denyDevice = (userCode: string) => send<unknown>("POST", "/enterprise/v1/auth/device/deny", { user_code: userCode });

export const changeLocalPassword = (username: string, currentPassword: string, newPassword: string) =>
	eapRequest<void>("/enterprise/v1/auth/local/password", {
		method: "POST",
		auth: "none",
		body: { username, current_password: currentPassword, new_password: newPassword },
	});

/** 可展示的错误文案：冻结契约的 message，附 trace id 便于排障。 */
export function problemText(error: unknown, fallback: string): string {
	if (error instanceof EapProblem) return error.traceId ? `${error.message}（trace ${error.traceId}）` : error.message;
	return fallback;
}
