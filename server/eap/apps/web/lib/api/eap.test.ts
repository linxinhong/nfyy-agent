import { afterEach, describe, expect, it, vi } from "vitest";
import { bindSessionBridge, EapProblem, eapRequest, localLogin } from "./eap";

function respond(status: number, body: unknown): void {
	vi.stubGlobal(
		"fetch",
		vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })),
	);
}

afterEach(() => {
	vi.unstubAllGlobals();
	bindSessionBridge({ readToken: () => undefined, onUnauthorized: () => undefined });
});

describe("eapRequest", () => {
	it("parses the frozen Problem schema into EapProblem", async () => {
		respond(403, {
			code: "workspace_forbidden",
			message: "principal is not a member of workspace finance",
			trace_id: "4bf92f3577b34da6a3ce929d0e0e4736",
			retriable: false,
		});
		const error = await eapRequest("/enterprise/v1/token/exchange", { method: "POST" }).catch((cause: unknown) => cause);
		expect(error).toBeInstanceOf(EapProblem);
		const problem = error as EapProblem;
		expect(problem.code).toBe("workspace_forbidden");
		expect(problem.traceId).toBe("4bf92f3577b34da6a3ce929d0e0e4736");
		expect(problem.retriable).toBe(false);
		expect(problem.status).toBe(403);
	});

	it("returns parsed JSON on success", async () => {
		respond(200, { user: { id: "u", display_name: "d", email: "e" }, workspaces: [], default_workspace: "" });
		const data = await eapRequest<{ user: { id: string } }>("/enterprise/v1/profile");
		expect(data.user.id).toBe("u");
	});

	it("maps non-JSON failures to a synthetic problem", async () => {
		vi.stubGlobal(
			"fetch",
			vi.fn(async () => new Response("boom", { status: 500 })),
		);
		const error = await eapRequest("/enterprise/v1/keys").catch((cause: unknown) => cause);
		expect(error).toBeInstanceOf(EapProblem);
		expect((error as EapProblem).code).toBe("http_500");
	});

	it("shows the login failure without clearing an existing session", async () => {
		const onUnauthorized = vi.fn();
		bindSessionBridge({ readToken: () => "old-session", onUnauthorized });
		respond(401, { code: "unauthenticated", message: "invalid username or password", trace_id: "login-trace" });
		const error = await localLogin("admin", "wrong").catch((cause: unknown) => cause);
		expect(error).toBeInstanceOf(EapProblem);
		expect((error as EapProblem).message).toBe("invalid username or password");
		expect((error as EapProblem).traceId).toBe("login-trace");
		expect(onUnauthorized).not.toHaveBeenCalled();
		expect(vi.mocked(fetch).mock.calls[0]?.[1]?.headers).not.toHaveProperty("Authorization");
	});

	it("clears an expired session on a protected request", async () => {
		const onUnauthorized = vi.fn();
		bindSessionBridge({ readToken: () => "old-session", onUnauthorized });
		respond(401, { code: "unauthenticated", message: "expired" });
		const error = await eapRequest("/enterprise/v1/profile").catch((cause: unknown) => cause);
		expect((error as EapProblem).message).toBe("会话已失效，请重新登录");
		expect(onUnauthorized).toHaveBeenCalledOnce();
	});
});
