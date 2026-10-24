/**
 * HTTP 客户端 —— 单一入口，统一错误归一化。
 *
 * 为什么不用 axios：fetch 够用，且少一个依赖。bifrost 用 axios 只在唯一一处
 * 跨域第三方调用（需要 maxRedirects / validateStatus 这些 fetch 没有的开箱能力）。
 *
 * 401 处理有一个逃生口 `suppressGlobal401`：某些页面自己处理未授权显示
 * （例如"链接已失效"视图），全局跳转会把用户从他刚打开的页面踢走。
 */

export interface ApiErrorShape {
	message: string;
	status?: number;
	code?: string;
	details?: unknown;
}

/** 归一化后的错误 —— 调用方只需处理这一种形状。 */
export class ApiError extends Error {
	readonly status?: number;
	readonly code?: string;
	readonly details?: unknown;

	constructor(shape: ApiErrorShape) {
		super(shape.message);
		this.name = "ApiError";
		this.status = shape.status;
		this.code = shape.code;
		this.details = shape.details;
	}
}

/** 401 全局跳转的抑制开关。自己处理未授权显示的页面在挂载时打开。 */
let suppressGlobal401 = false;

export function setSuppressGlobal401(value: boolean) {
	suppressGlobal401 = value;
}

function handleUnauthorized() {
	if (suppressGlobal401) return;
	const target = `${window.location.pathname}${window.location.search}`;
	window.location.href = `/login?goto=${encodeURIComponent(target)}`;
}

export interface RequestOptions extends Omit<RequestInit, "body"> {
	/** 对象会被 JSON 序列化；FormData 原样传（浏览器自己带 multipart boundary）。 */
	body?: unknown;
	/** 追加到 URL 的查询参数。undefined / null 的键被跳过。 */
	searchParams?: Record<string, string | number | boolean | undefined | null>;
}

const BASE_URL = "/api";

function buildUrl(path: string, searchParams?: RequestOptions["searchParams"]): string {
	const url = `${BASE_URL}${path.startsWith("/") ? path : `/${path}`}`;
	if (!searchParams) return url;
	const qs = new URLSearchParams();
	for (const [key, value] of Object.entries(searchParams)) {
		if (value === undefined || value === null) continue;
		qs.set(key, String(value));
	}
	const serialized = qs.toString();
	return serialized ? `${url}?${serialized}` : url;
}

async function parseErrorBody(response: Response): Promise<ApiErrorShape> {
	const status = response.status;
	try {
		const data: unknown = await response.json();
		if (data && typeof data === "object") {
			const record = data as Record<string, unknown>;
			// 依次尝试常见的错误字段命名，全都不中才落到 statusText
			const message =
				(typeof record.message === "string" && record.message) ||
				(typeof record.error === "string" && record.error) ||
				(typeof record.detail === "string" && record.detail) ||
				response.statusText;
			return {
				message,
				status,
				code: typeof record.code === "string" ? record.code : undefined,
				details: record.details,
			};
		}
	} catch {
		// 响应体不是 JSON —— 用 statusText
	}
	return { message: response.statusText || `请求失败（${status}）`, status };
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
	const { body, searchParams, headers, ...rest } = options;

	const isFormData = body instanceof FormData;
	const finalHeaders = new Headers(headers);
	// FormData 时不能设 Content-Type —— 浏览器要自己加 multipart boundary
	if (!isFormData && body !== undefined && !finalHeaders.has("Content-Type")) {
		finalHeaders.set("Content-Type", "application/json");
	}

	let response: Response;
	try {
		response = await fetch(buildUrl(path, searchParams), {
			credentials: "include",
			...rest,
			headers: finalHeaders,
			body: isFormData ? body : body !== undefined ? JSON.stringify(body) : undefined,
		});
	} catch (cause) {
		// 网络层失败（离线、DNS、CORS）—— 与 HTTP 错误区分开，文案不同
		throw new ApiError({
			message: "网络错误：无法连接服务器",
			code: "NETWORK_ERROR",
			details: cause,
		});
	}

	if (response.status === 401) {
		handleUnauthorized();
		throw new ApiError({ message: "未授权", status: 401, code: "UNAUTHORIZED" });
	}

	if (!response.ok) {
		throw new ApiError(await parseErrorBody(response));
	}

	if (response.status === 204) return undefined as T;

	const contentType = response.headers.get("Content-Type") ?? "";
	if (!contentType.includes("application/json")) {
		return (await response.text()) as T;
	}
	return (await response.json()) as T;
}

export const api = {
	get: <T>(path: string, options?: Omit<RequestOptions, "body" | "method">) => request<T>(path, { ...options, method: "GET" }),
	post: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, "body" | "method">) =>
		request<T>(path, { ...options, method: "POST", body }),
	put: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, "body" | "method">) =>
		request<T>(path, { ...options, method: "PUT", body }),
	patch: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, "body" | "method">) =>
		request<T>(path, { ...options, method: "PATCH", body }),
	delete: <T>(path: string, options?: Omit<RequestOptions, "body" | "method">) => request<T>(path, { ...options, method: "DELETE" }),
};

/** 从任意 unknown 错误里取一条可展示的消息。 */
export function getErrorMessage(error: unknown): string {
	if (error instanceof ApiError) return error.message;
	if (error instanceof Error) return error.message;
	if (typeof error === "string") return error;
	return "发生未知错误";
}
