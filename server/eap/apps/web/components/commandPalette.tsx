import { createContext, useContext, useMemo, useState, type ReactNode } from "react";
import { useNavigate } from "@tanstack/react-router";
import { useHotkeys } from "react-hotkeys-hook";
import { useI18n } from "@/components/i18nProvider";
import { Clock, FileText } from "lucide-react";
import {
	CommandDialog,
	CommandEmpty,
	CommandGroup,
	CommandInput,
	CommandItem,
	CommandList,
	CommandSeparator,
} from "@/components/ui/command";
import { flattenPages } from "@/lib/constants/nav";
import { usePermissions } from "@/lib/auth/permissions";
import { useRecentPages } from "@/lib/hooks/useRecentPages";

/**
 * ⌘K 命令面板 —— 全局唤起，页面跳转 + 最近访问。
 *
 * open 状态放 context 而非组件内：热键（全局）、侧栏搜索按钮、以及将来任何
 * "打开命令面板"的入口都要能触发同一个面板。
 *
 * 面板本身是 portal 居中浮层（CommandDialog），不占布局。
 *
 * 接入业务检索：在下方 CommandGroup 里加一组（如"告警""订单""用户"），
 * onSelect 深链到对应页面。见文件末尾 TODO。
 */
interface CommandPaletteValue {
	open: boolean;
	setOpen: (open: boolean) => void;
}

const CommandPaletteContext = createContext<CommandPaletteValue | null>(null);

export function useCommandPalette(): CommandPaletteValue {
	const ctx = useContext(CommandPaletteContext);
	if (!ctx) throw new Error("useCommandPalette 必须在 CommandPaletteProvider 内使用");
	return ctx;
}

export function CommandPaletteProvider({ children }: { children: ReactNode }) {
	const [open, setOpen] = useState(false);
	const value = useMemo(() => ({ open, setOpen }), [open]);
	return (
		<CommandPaletteContext.Provider value={value}>
			{children}
			<CommandPalette />
		</CommandPaletteContext.Provider>
	);
}

function CommandPalette() {
	const { t } = useI18n();
	const { open, setOpen } = useCommandPalette();
	const navigate = useNavigate();
	const { recent } = useRecentPages();
	const { has } = usePermissions();
	const pages = flattenPages(has);
	useHotkeys(
		"shift+mod+p",
		(event) => {
			event.preventDefault();
			setOpen(!open);
		},
		{ enableOnFormTags: true, enableOnContentEditable: true },
		[open, setOpen],
	);

	/*
	 * ⌘K 唤起。enableOnFormTags:true 让它在任何地方（包括页面内的搜索框里打字时）
	 * 都能开面板 —— ⌘K 是带修饰键的组合键，不会与普通输入冲突。
	 * 面板内的检索由 cmdk 自己的 CommandInput 处理，与此热键无关。
	 */
	useHotkeys(
		"mod+k",
		(e) => {
			e.preventDefault();
			setOpen(!open);
		},
		{ enableOnFormTags: true, enableOnContentEditable: true },
		[open, setOpen],
	);

	const go = (to: string, search?: Record<string, unknown>) => {
		setOpen(false);
		// 命令面板跳任意注册路由，to 是运行时字符串，无法静态收窄到联合类型
		void navigate({ to: to as never, search: search as never });
	};

	const recentPages = recent.map((path) => pages.find((p) => p.to === path)).filter((p): p is (typeof pages)[number] => p !== undefined);

	return (
		<CommandDialog open={open} onOpenChange={setOpen} title={t("command.title")} description={t("command.searchPlaceholder")}>
			<CommandInput placeholder={t("command.searchPlaceholder")} />
			<CommandList>
				<CommandEmpty>{t("command.noResults")}</CommandEmpty>

				{recentPages.length > 0 && (
					<>
						<CommandGroup heading={t("command.recent")}>
							{recentPages.map((p) => (
								<CommandItem key={`recent-${p.key}`} value={`最近 ${p.label}`} onSelect={() => go(p.to)}>
									<Clock className="text-muted-foreground" />
									<span>{p.label}</span>
									<span className="text-muted-foreground ml-auto text-xs">{p.groupLabel}</span>
								</CommandItem>
							))}
						</CommandGroup>
						<CommandSeparator />
					</>
				)}

				<CommandGroup heading={t("command.pages")}>
					{pages.map((p) => (
						<CommandItem key={p.key} value={`${p.label} ${p.groupLabel}`} onSelect={() => go(p.to)}>
							<FileText className="text-muted-foreground" />
							<span>{p.label}</span>
							<span className="text-muted-foreground ml-auto text-xs">{p.groupLabel}</span>
						</CommandItem>
					))}
				</CommandGroup>

				{/*
					TODO 接入业务检索：在此加一组，例如
					<CommandGroup heading="告警">
						{matches.map((a) => (
							<CommandItem key={a.id} value={`${a.title} ${a.id}`}
								onSelect={() => go("/workspace/alerts", { q: a.id })}>…</CommandItem>
						))}
					</CommandGroup>
					接真实后端时把 matches 换成搜索接口的结果。
				*/}
			</CommandList>
		</CommandDialog>
	);
}
