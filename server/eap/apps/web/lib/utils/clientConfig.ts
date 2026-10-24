/**
 * 客户端配置生成 —— Agent Center / 密钥页共用的分发物。
 *
 * 安全边界（ADR-0003）：配置文件里**只有端点与说明，没有任何密钥**。
 * 员工把 agk_* key 粘贴进客户端本地的安全存储；配置文件本身可以随意
 * 存放与分享，泄露无害。
 */

export interface ClientConfig {
	base_url: string;
	workspace: string;
	api_key_env: string;
	protocol: {
		openai: string;
		anthropic: string;
		mcp: string;
		enterprise: string;
	};
	instructions: string;
}

export function buildClientConfig(baseUrl: string, workspace: string): ClientConfig {
	const origin = baseUrl.replace(/\/+$/, "");
	return {
		base_url: origin,
		workspace,
		api_key_env: "EAP_API_KEY",
		protocol: {
			openai: `${origin}/v1`,
			anthropic: `${origin}/v1/messages`,
			mcp: `${origin}/mcp/{server}`,
			enterprise: `${origin}/enterprise/v1`,
		},
		instructions: "将你的 EAP API Key 设置到环境变量 EAP_API_KEY；本配置文件不含任何密钥，可安全保存与分享。",
	};
}

/** 触发浏览器下载。文件名带 workspace 便于多 Workspace 员工区分。 */
export function downloadClientConfig(config: ClientConfig): void {
	const blob = new Blob([JSON.stringify(config, null, "\t")], { type: "application/json" });
	const url = URL.createObjectURL(blob);
	const anchor = document.createElement("a");
	anchor.href = url;
	anchor.download = `eap-client-config-${config.workspace || "default"}.json`;
	document.body.append(anchor);
	anchor.click();
	anchor.remove();
	URL.revokeObjectURL(url);
}
