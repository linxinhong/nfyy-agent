/**
 * 图表色 —— 从 CSS 变量读取，**不硬编码 hex**。
 *
 * 这是本模板对 bifrost 的一处修正：bifrost 在 globals.css 定义了
 * --chart-1..5（含暗色覆盖）但零消费，真实调色板硬编码在 chartUtils.ts 里，
 * 代价是图表色不随明暗切换。
 *
 * 为什么需要运行时读取：recharts 的 fill/stroke 是 SVG 属性而非 className，
 * 吃不到 Tailwind 工具类。所以必须把 CSS 变量解析成实际色值再传给它。
 *
 * 为什么用 hook 而非模块常量：主题切换时色值要跟着变。模块常量在首次
 * import 时求值一次，之后切到暗色不会更新。
 */
import { useEffect, useState } from "react";

/** 分类序列色 —— 用于「多个同类系列」，无语义含义，按索引取模循环。 */
const CATEGORICAL_VARS = ["--chart-1", "--chart-2", "--chart-3", "--chart-4", "--chart-5"] as const;

/** 语义色 —— 有明确含义，绝不用于分类。 */
const SEMANTIC_VARS = {
	success: "--success",
	warning: "--warning",
	danger: "--destructive",
	info: "--info",
	primary: "--primary",
	muted: "--muted-foreground",
} as const;

export type SemanticColorKey = keyof typeof SEMANTIC_VARS;

function readVar(name: string): string {
	if (typeof window === "undefined") return "";
	return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export interface ChartColors {
	categorical: string[];
	semantic: Record<SemanticColorKey, string>;
	/** 按索引取分类色，自动取模 —— 系列数超过调色板长度时循环复用。 */
	byIndex: (index: number) => string;
}

/**
 * 订阅主题变化并返回当前的图表色。
 *
 * 监听 documentElement 的 class 变化 —— next-themes 通过增删 `dark` 类切换主题，
 * 所以 MutationObserver 是可靠的信号源（比监听 matchMedia 更准，因为用户可以
 * 手动选择明/暗而不跟随系统）。
 */
export function useChartColors(): ChartColors {
	const [colors, setColors] = useState<ChartColors>(() => resolve());

	useEffect(() => {
		const observer = new MutationObserver(() => setColors(resolve()));
		observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
		return () => observer.disconnect();
	}, []);

	return colors;
}

function resolve(): ChartColors {
	const categorical = CATEGORICAL_VARS.map(readVar);
	const semantic = Object.fromEntries(Object.entries(SEMANTIC_VARS).map(([key, cssVar]) => [key, readVar(cssVar)])) as Record<
		SemanticColorKey,
		string
	>;

	return {
		categorical,
		semantic,
		byIndex: (index: number) => categorical[index % categorical.length] ?? semantic.primary,
	};
}

/**
 * 分类调色板长度 —— 供 top-N 聚合逻辑参考。
 *
 * 注意**不要**把它直接当作 top-N 阈值（bifrost 就是这么做的：
 * `TOP_SERIES_LIMIT = MODEL_COLORS.length`）。那样加一个颜色就会静默改变
 * 数据聚合行为，两个本该无关的决定被绑在了一起。
 * 需要 top-N 时单独定义阈值常量，超出调色板长度就循环复用颜色。
 */
export const CATEGORICAL_COLOR_COUNT = CATEGORICAL_VARS.length;
