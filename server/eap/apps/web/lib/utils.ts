import { clsx, type ClassValue } from "clsx";
import type React from "react";
import { twMerge } from "tailwind-merge";

/**
 * cn —— 唯一的 className 合并入口。
 *
 * clsx 处理条件类，twMerge 消解 Tailwind 冲突（后写的胜）。二者顺序不能颠倒：
 * twMerge 需要拿到已拼好的字符串才能判断哪些类互斥。
 *
 * 注意：bifrost 里 cn() 有两份定义（lib/utils.ts 与 components/ui/utils.ts），
 * 45 个组件用前者、2 个用后者。本模板只保留这一份。
 */
export function cn(...inputs: ClassValue[]) {
	return twMerge(clsx(inputs));
}

/**
 * Radix Dialog 内输入框的失焦兜底。
 *
 * Radix 的 Dialog/Tabs 关闭浮层时会把焦点抢回容器，导致 Dialog 里的 input 失焦后
 * 焦点落在 role="dialog" 上而非回到原处。这里把焦点显式还给 relatedTarget，
 * 但跳过 dialog/tabpanel 自身 —— 否则会与 Radix 自己的焦点管理互相抢夺形成循环。
 */
export const radixDialogOnBlurWorkaround = (event: React.FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => {
	const element = event.relatedTarget as HTMLElement;
	if (element?.getAttribute("role") === "dialog" || element?.getAttribute("role") === "tabpanel") return;
	element?.focus();
};
