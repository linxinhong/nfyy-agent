import { X, type LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

/**
 * 无业务依赖的页内工作区标签栏；调用方定义标签内容与关闭规则。
 *
 * 视觉范式：下划线式标签（GitHub / 浏览器设置页）——激活 = 深色加粗 +
 * 主色 2px 下划线，非激活 = 灰字悬停变深。下划线用 -mb-px 盖住容器
 * 分隔线，形成"标签长在内容区上"的从属关系，替代旧版弱胶囊样式。
 */
export interface WorkspaceTab {
	id: string;
	label: string;
	/** 类型图标（列表 ≣ / 来电 ☎ / 任务 📋 …）——调用方传入，降低辨认成本。 */
	icon?: LucideIcon;
	closable?: boolean;
}

export function WorkspaceTabBar({
	tabs,
	activeId,
	onActivate,
	onClose,
}: {
	tabs: readonly WorkspaceTab[];
	activeId: string;
	onActivate: (id: string) => void;
	onClose?: (id: string) => void;
}) {
	return (
		<div className="custom-scrollbar mb-2 flex h-8 shrink-0 items-end gap-1.5 overflow-x-auto border-b px-1" data-testid="workspace-tabs">
			{tabs.map((tab) => {
				const active = tab.id === activeId;
				const Icon = tab.icon;
				return (
					<div
						key={tab.id}
						className={cn(
							"group/tab -mb-px flex h-8 shrink-0 items-center gap-1.5 border-b-2 px-2.5 text-sm transition-colors",
							active ? "border-primary text-foreground font-medium" : "text-muted-foreground border-transparent hover:text-foreground",
						)}
					>
						<button
							type="button"
							onClick={() => onActivate(tab.id)}
							className="flex max-w-40 items-center gap-1.5 truncate"
							aria-current={active ? "page" : undefined}
						>
							{Icon && <Icon className="size-3.5 shrink-0" aria-hidden="true" />}
							<span className="truncate">{tab.label}</span>
						</button>
						{tab.closable && onClose && (
							<button
								type="button"
								onClick={() => onClose(tab.id)}
								aria-label={`关闭 ${tab.label}`}
								className="hover:bg-muted hover:text-foreground flex size-5 shrink-0 items-center justify-center rounded-full"
							>
								<X className="size-3" />
							</button>
						)}
					</div>
				);
			})}
		</div>
	);
}
