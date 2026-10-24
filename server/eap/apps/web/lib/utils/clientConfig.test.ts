import { describe, expect, it } from "vitest";
import { buildClientConfig } from "./clientConfig";

describe("buildClientConfig", () => {
	it("exposes endpoints but never a key", () => {
		const config = buildClientConfig("https://agent.company.com", "secops");
		expect(config.base_url).toBe("https://agent.company.com");
		expect(config.workspace).toBe("secops");
		expect(config.api_key_env).toBe("EAP_API_KEY");
		expect(config.protocol.openai).toBe("https://agent.company.com/v1");
		expect(config.protocol.anthropic).toBe("https://agent.company.com/v1/messages");
		expect(config.protocol.mcp).toBe("https://agent.company.com/mcp/{server}");
		expect(config.protocol.enterprise).toBe("https://agent.company.com/enterprise/v1");
	});

	it("never contains credential material", () => {
		const config = buildClientConfig("http://127.0.0.1:18080", "finance");
		const serialized = JSON.stringify(config);
		expect(serialized).not.toContain("agk_");
		expect(serialized).not.toContain("token");
		expect(serialized).not.toContain("Authorization");
	});

	it("strips trailing slashes from the base url", () => {
		const config = buildClientConfig("https://agent.company.com///", "secops");
		expect(config.base_url).toBe("https://agent.company.com");
	});
});
