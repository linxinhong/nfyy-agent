/**
 * 配色包 —— 品牌色（--primary 轴）的可切换预设。
 *
 * 单一事实源：切换 UI 从这里渲染选项，globals.css 里每个 key 对应一段
 * `[data-palette="..."]` 规则。加配色包 = 这里加一项 + globals.css 加一段。
 *
 * 只换品牌色轴，中性色与语义色（success/warning/destructive/info）不动 ——
 * 后者的 WCAG 对比度关系已复算，配色包不该误伤。
 *
 * preview 是给切换菜单画色点用的（明色下的 primary），不参与实际渲染 ——
 * 实际颜色由 globals.css 的 CSS 变量控制，这里只是 UI 预览。
 */
export interface Palette {
	key: string;
	label: string;
	/** 菜单色点用（明色 primary 的近似值）。 */
	preview: string;
}

export const PALETTES: Palette[] = [
	{ key: "emerald", label: "翡翠绿", preview: "oklch(0.5081 0.1049 165.61)" },
	{ key: "blue", label: "宝石蓝", preview: "oklch(0.55 0.2 255)" },
	{ key: "violet", label: "紫罗兰", preview: "oklch(0.55 0.22 295)" },
	{ key: "amber", label: "琥珀橙", preview: "oklch(0.62 0.17 65)" },
];

export const DEFAULT_PALETTE = "emerald";

/** localStorage key —— 与 index.html 预探测脚本共用同一个字符串。 */
export const PALETTE_STORAGE_KEY = "palette";

export function isValidPalette(key: string | null | undefined): key is string {
	return typeof key === "string" && PALETTES.some((p) => p.key === key);
}
