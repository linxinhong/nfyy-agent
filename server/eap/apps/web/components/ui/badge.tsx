import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";

import { cn } from "@/lib/utils";

/**
 * Badge —— 四个语义轴全部走 token，无一处 Tailwind 调色板直用。
 *
 * 这是本模板对 bifrost 的主要修正。bifrost 的 success/warning variant 用
 * `bg-green-100 text-black` / `bg-yellow-100 text-black`，两个问题：
 *   · 无暗色变体 —— 暗色模式下仍是浅绿浅黄底配黑字，明显断层
 *   · text-black 写死 —— 不随主题切换
 * 根因是它的 globals.css 缺 --success / --warning / --info，只有 --primary
 * 和 --destructive 两个轴，于是全仓 1289 处直接用调色板。
 *
 * 每个语义轴三个 token：
 *   -muted       弱底色
 *   -foreground  压在弱底上的文字色（与弱底对比度须 ≥ 4.5:1）
 *   主色         边框（配 /50 透明度）
 */
const badgeVariants = cva(
	"inline-flex items-center justify-center rounded-sm border px-2 py-0.5 text-xs font-medium w-fit whitespace-nowrap shrink-0 [&>svg]:size-3 gap-1 [&>svg]:pointer-events-none focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px] transition-[color,box-shadow] overflow-hidden",
	{
		variants: {
			variant: {
				default: "bg-primary/10 border-primary/50 text-primary",
				secondary: "border-transparent bg-secondary text-secondary-foreground",
				outline: "border-border text-foreground",
				success: "bg-success-muted border-success/50 text-success-foreground",
				warning: "bg-warning-muted border-warning/50 text-warning-foreground",
				destructive: "bg-destructive-muted border-destructive/50 text-destructive-foreground",
				info: "bg-info-muted border-info/50 text-info-foreground",
			},
		},
		defaultVariants: {
			variant: "default",
		},
	},
);

function Badge({
	className,
	variant,
	asChild = false,
	...props
}: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants> & { asChild?: boolean }) {
	const Comp = asChild ? Slot : "span";

	return <Comp data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />;
}

export { Badge, badgeVariants };
