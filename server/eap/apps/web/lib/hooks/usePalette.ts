import { useCallback, useSyncExternalStore } from "react";
import { DEFAULT_PALETTE, PALETTE_STORAGE_KEY, isValidPalette } from "@/lib/constants/palettes";

/**
 * 配色包状态 —— 读写 localStorage + 设 <html data-palette>。
 *
 * 与明暗（next-themes）正交、机制平行：next-themes 管 class，这里管 data-palette。
 * 不复用 next-themes 是因为它只认单一 attribute，塞两个维度会打架。
 *
 * 用 useSyncExternalStore 而非 useState：data-palette 可能被 index.html 的
 * 预探测脚本在 React 之前设好，事实源在 DOM/localStorage 而非组件里。
 */
function readPalette(): string {
	if (typeof document === "undefined") return DEFAULT_PALETTE;
	const fromDom = document.documentElement.dataset.palette;
	if (isValidPalette(fromDom)) return fromDom;
	try {
		const stored = localStorage.getItem(PALETTE_STORAGE_KEY);
		if (isValidPalette(stored)) return stored;
	} catch {
		// localStorage 不可用
	}
	return DEFAULT_PALETTE;
}

// 模块级订阅者集合 —— setPalette 后通知所有 usePalette 实例
const listeners = new Set<() => void>();

function subscribe(cb: () => void) {
	listeners.add(cb);
	return () => listeners.delete(cb);
}

export function usePalette() {
	const palette = useSyncExternalStore(subscribe, readPalette, () => DEFAULT_PALETTE);

	const setPalette = useCallback((key: string) => {
		if (!isValidPalette(key)) return;
		// 默认配色不写 data 属性也不留 localStorage —— 保持 DOM 干净，emerald 走 :root
		if (key === DEFAULT_PALETTE) {
			delete document.documentElement.dataset.palette;
			try {
				localStorage.removeItem(PALETTE_STORAGE_KEY);
			} catch {
				/* ignore */
			}
		} else {
			document.documentElement.dataset.palette = key;
			try {
				localStorage.setItem(PALETTE_STORAGE_KEY, key);
			} catch {
				/* ignore */
			}
		}
		for (const cb of listeners) cb();
	}, []);

	return { palette, setPalette };
}
