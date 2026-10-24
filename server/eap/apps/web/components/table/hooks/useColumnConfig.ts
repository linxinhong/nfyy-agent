import { type ColumnPinningState, type VisibilityState } from "@tanstack/react-table";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef } from "react";

export interface ColumnConfigEntry {
	id: string;
	visible: boolean;
	pinned: "left" | "right" | false;
}

interface UseColumnConfigOptions {
	/** All available column IDs in their default order */
	columnIds: string[];
	/** URL query param name for persistence */
	paramName?: string;
	/**
	 * 不参与配置的列（永远可见、位置固定）。
	 *
	 * **调用方必须传稳定引用**（模块级常量或 useMemo）。内联字面量
	 * `fixedColumns={{ left: ["id"] }}` 每帧产生新对象，会让本 hook 内部所有
	 * 记忆化失效，表格每帧重算列顺序与固定偏移。
	 */
	fixedColumns?: { left?: string[]; right?: string[] };
	/**
	 * localStorage key for persisting the config across sessions. When set, the
	 * stored config seeds the table on load if the URL param is empty (URL wins).
	 */
	storageKey?: string;
	/** Configurable column IDs that should be hidden by default (when not in URL/localStorage). */
	defaultHidden?: string[];
}

// URL format: col1,col2:h,col3:l,col4:r
// no suffix = visible & unpinned, :h = hidden, :l = pinned left, :r = pinned right
function serialize(entries: ColumnConfigEntry[]): string {
	return entries
		.map((e) => {
			let flags = "";
			if (!e.visible) flags += "h";
			if (e.pinned === "left") flags += "l";
			if (e.pinned === "right") flags += "r";
			const encoded = encodeURIComponent(e.id);
			return flags ? `${encoded}:${flags}` : encoded;
		})
		.join(",");
}

function deserialize(str: string): ColumnConfigEntry[] {
	if (!str) return [];
	return str.split(",").map((part) => {
		const [rawId = "", flags = ""] = part.split(":");
		const id = decodeURIComponent(rawId);
		return {
			id,
			visible: !flags.includes("h"),
			pinned: flags.includes("l") ? ("left" as const) : flags.includes("r") ? ("right" as const) : false,
		};
	});
}

function isSerializedConfigValid(str: string): boolean {
	try {
		return str.split(",").every((part) => {
			const [rawId = "", flags = ""] = part.split(":");
			return rawId !== "" && decodeURIComponent(rawId) !== "" && /^[hlr]*$/.test(flags) && !(flags.includes("l") && flags.includes("r"));
		});
	} catch {
		return false;
	}
}

/**
 * 列配置持久化 —— URL 为主、localStorage 兜底。
 *
 * URL 格式紧凑到能直接分享：`col1,col2:h,col3:l,col4:r`
 *   无后缀 = 可见未固定 · `:h` = 隐藏 · `:l` = 左固定 · `:r` = 右固定
 *
 * 双层存储的优先级是 **URL 胜**：URL 里有配置就用它（分享链接必须还原成对方看到的样子），
 * 没有才拿 localStorage 里的个人偏好来 seed。
 *
 * URL 状态走 Router 原生 `useSearch`/`navigate`，不引 nuqs —— 同一份 URL 状态两个
 * 事实源严格更差，且 nuqs 与 Router 共存需要三处补丁（覆写 parseSearch/stringifySearch、
 * 自定义 encode 以避开被当成路径分隔符的 `/`、处理 clearOnDefault 与 localStorage 互相覆盖）。
 *
 * 调用方必须在路由的 `validateSearch` 里声明该参数，例如：
 * ```ts
 * validateSearch: (raw: Record<string, unknown>) => ({
 *   cols: typeof raw.cols === "string" ? raw.cols : undefined,
 * })
 * ```
 */
export function useColumnConfig({ columnIds, paramName = "cols", fixedColumns, storageKey, defaultHidden }: UseColumnConfigOptions) {
	const navigate = useNavigate();
	// strict:false —— 该 hook 可被任意路由下的表格调用，不绑定具体路由的 search 类型。
	const search = useSearch({ strict: false }) as Record<string, unknown>;
	const rawValue = search[paramName];
	const raw = typeof rawValue === "string" ? rawValue : "";

	const setRaw = useCallback(
		(next: string | null) => {
			void navigate({
				// @ts-expect-error —— 泛型路由下无法静态知道 search 形状；运行时只改这一个键。
				search: (prev: Record<string, unknown>) => ({ ...prev, [paramName]: next ?? undefined }),
				replace: true, // 列配置不该污染浏览器返回栈
			});
		},
		[navigate, paramName],
	);

	// `fixedColumns?.left ?? []` 每次渲染都产生新数组引用，若直接放进依赖数组，
	// 下游所有 useMemo 每帧都会重算 —— 记忆化完全失效（bifrost 原版就有这个问题，
	// oxlint 的 exhaustive-deps 抓到了）。用 useMemo 稳定住引用，依赖对象本身。
	const fixedLeft = useMemo(() => fixedColumns?.left ?? [], [fixedColumns]);
	const fixedRight = useMemo(() => fixedColumns?.right ?? [], [fixedColumns]);
	const fixedSet = useMemo(() => new Set([...fixedLeft, ...fixedRight]), [fixedLeft, fixedRight]);

	const configurableIds = useMemo(() => columnIds.filter((id) => !fixedSet.has(id)), [columnIds, fixedSet]);

	const defaultHiddenSet = useMemo(() => new Set(defaultHidden ?? []), [defaultHidden]);

	// 从 localStorage seed 一次 —— 仅当 URL 无配置时（URL 胜）。
	//
	// 为什么用 ref 而非依赖数组：seed 只能发生一次。若跟着 raw 变化重跑，用户主动清空
	// 列配置后会被 localStorage 里的旧值立刻写回，表现为"清不掉"。
	// Router 的 search 在首次渲染时已同步可用（不像某些库要等一帧 hydrate），
	// 所以这里读到的 raw 是真实值，不会误判成空。
	const seededRef = useRef(false);
	useEffect(() => {
		if (seededRef.current || !storageKey) return;
		seededRef.current = true;
		if (raw) return; // URL 里的显式配置优先
		try {
			const stored = localStorage.getItem(storageKey);
			if (!stored) return;
			if (isSerializedConfigValid(stored)) {
				setRaw(stored);
			} else {
				// 损坏的值会让每次加载都失败一次，清掉而不是反复重试
				localStorage.removeItem(storageKey);
			}
		} catch {
			// localStorage 不可用（隐私模式）—— 保持默认，不是错误
		}
	}, [raw, storageKey, setRaw]);

	// Merge URL config with available columns (handles added/removed columns)
	const entries = useMemo(() => {
		const parsed = deserialize(raw);
		const result: ColumnConfigEntry[] = [];
		const seen = new Set<string>();

		// Columns present in URL config that still exist
		for (const entry of parsed) {
			if (configurableIds.includes(entry.id) && !seen.has(entry.id)) {
				result.push(entry);
				seen.add(entry.id);
			}
		}

		// New columns not yet in URL config
		for (const id of configurableIds) {
			if (!seen.has(id)) {
				result.push({ id, visible: !defaultHiddenSet.has(id), pinned: false });
			}
		}

		return result;
	}, [raw, configurableIds, defaultHiddenSet]);

	// TanStack table state
	const columnOrder = useMemo(() => [...fixedLeft, ...entries.map((e) => e.id), ...fixedRight], [entries, fixedLeft, fixedRight]);

	const columnVisibility = useMemo(() => {
		const vis: VisibilityState = {};
		for (const entry of entries) {
			if (!entry.visible) vis[entry.id] = false;
		}
		return vis;
	}, [entries]);

	const columnPinning = useMemo(
		(): ColumnPinningState => ({
			left: [...fixedLeft, ...entries.filter((e) => e.pinned === "left").map((e) => e.id)],
			right: [...entries.filter((e) => e.pinned === "right").map((e) => e.id), ...fixedRight],
		}),
		[entries, fixedLeft, fixedRight],
	);

	const persist = useCallback(
		(newEntries: ColumnConfigEntry[]) => {
			const serialized = serialize(newEntries);
			setRaw(serialized || null);
			if (storageKey) {
				try {
					if (serialized) localStorage.setItem(storageKey, serialized);
					else localStorage.removeItem(storageKey);
				} catch {
					// localStorage unavailable — preference won't persist.
				}
			}
		},
		[setRaw, storageKey],
	);

	const toggleVisibility = useCallback(
		(columnId: string) => {
			persist(entries.map((e) => (e.id === columnId ? { ...e, visible: !e.visible } : e)));
		},
		[entries, persist],
	);

	const togglePin = useCallback(
		(columnId: string, position: "left" | "right") => {
			persist(entries.map((e) => (e.id === columnId ? { ...e, pinned: e.pinned === position ? false : position } : e)));
		},
		[entries, persist],
	);

	const reorder = useCallback(
		(newEntries: ColumnConfigEntry[]) => {
			persist(newEntries);
		},
		[persist],
	);

	const reset = useCallback(() => {
		setRaw(null);
		if (storageKey) {
			try {
				localStorage.removeItem(storageKey);
			} catch {
				// localStorage unavailable — nothing to clear.
			}
		}
	}, [setRaw, storageKey]);

	return {
		entries,
		columnOrder,
		columnVisibility,
		columnPinning,
		toggleVisibility,
		togglePin,
		reorder,
		reset,
	};
}
